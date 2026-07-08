package runtime

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// bindCodeAlphabet 是 owner 绑定码使用的 base32 字母表，去掉了易混字符 0/O/1/I，
// 方便用户在 IM 里手动输入。共 32 个字符（2^5），可整字节映射。
const bindCodeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// bindCommandVerb 是 owner 绑定指令的动词部分（不含分隔空白）。
const bindCommandVerb = "/bind"

// parseBindCommand 识别 "/bind <token>" 指令。
//
// 返回 token, ok：ok=true 表示这是一条绑定指令，ok=false 表示不是（应放行进 agent turn）。
// 空白被容忍（空格/制表/换行均可分隔）；"/bind"（无参）不算绑定指令（让 agent 解释绑定用法）。
// 不校验 token 格式——校验是 handleOwnerBind 的事。
//
// coverage: contract (100% required)
func parseBindCommand(msg string) (token string, ok bool) {
	msg = strings.TrimSpace(msg)
	rest, matched := strings.CutPrefix(msg, bindCommandVerb)
	if !matched {
		return "", false
	}
	// /bind 后必须紧跟一个空白分隔符（或就是裸 /bind）。
	if rest != "" && !isASCIISpace(rest[0]) {
		return "", false // 例如 "/binder" 不算绑定指令
	}
	token = strings.TrimSpace(rest)
	if token == "" {
		return "", false
	}
	// 取第一个空白分隔的字段，容忍用户粘贴时带了换行或多余文本。
	if i := strings.IndexAny(token, " \t\n\r"); i >= 0 {
		token = token[:i]
	}
	return token, true
}

func isASCIISpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// IsBindCommand 报告 msg 是否是一条 /bind 绑定指令（含合法 token）。
// 供 channel runner 做 pre-auth 放行：配了 allowed_senders 的入口，
// 未授权 sender 发 /bind <code> 也要能进入绑定流程（否则安全入口永远无法首次绑定）。
// 仅检查「是不是 /bind 指令」，不关心 token 内容——token 验证在 service 层 handleOwnerBind。
//
// 注意："/bind"（无参）不算绑定指令（返回 false），会走正常授权拒绝路径。
func IsBindCommand(msg string) bool {
	_, ok := parseBindCommand(msg)
	return ok
}

// generateBindCode 生成一个 8 字符的 owner 绑定码，分两组用 "-" 连接，形如 "WDJM-LHKD"。
//
// 用 crypto/rand（仓库首次引入），5 字节随机 → base32（去易混字符）→ 8 字符。
// 熵 ≈ 40 bit，配合「一次性消费 + 已绑即废」，爆破不现实。
//
// coverage: contract (100% required)
func generateBindCode() string {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败极其罕见；panic 是诚实契约——绑定码无法安全生成时不应静默继续。
		panic("owner_bind: crypto/rand failed: " + err.Error())
	}
	return encodeBindCode(b[:])
}

// encodeBindCode 把 5 字节编码成 8 字符绑定码（带分隔符）。
// 5 字节 = 40 bit = 8 个 5-bit 组 = 8 个 base32 字符。
func encodeBindCode(b []byte) string {
	bits := uint64(b[0])<<32 | uint64(b[1])<<24 | uint64(b[2])<<16 | uint64(b[3])<<8 | uint64(b[4])
	out := make([]byte, 0, 9)
	for i := 0; i < 8; i++ {
		// 从高位向低位，每 5 bit 取一个
		shift := 35 - 5*i
		idx := (bits >> shift) & 0x1f
		out = append(out, bindCodeAlphabet[idx])
		if i == 3 {
			out = append(out, '-')
		}
	}
	return string(out)
}

// ownerBindingsFilename 是持久化 owner 绑定关系的文件名（放在 stateDir 根目录）。
const ownerBindingsFilename = "owner-bindings.json"

// ownerBinding 描述一条已建立的 ownership 关系：某 entrypoint 绑定了一个 owner sender。
type ownerBinding struct {
	EntrypointID  string    `json:"entrypoint_id"`
	OwnerSenderID string    `json:"owner_sender_id"`
	BoundAt       time.Time `json:"bound_at"`
}

// ownerBindingsFile 是 owner-bindings.json 的磁盘格式。
type ownerBindingsFile struct {
	Bindings []ownerBinding `json:"bindings"`
}

// ownerBindingStore 是 owner 绑定关系的内存缓存 + 持久化存储。
//
// 并发模型：用 RWMutex。读路径（Get/IsBound，被 IsOwner 授权热路径调用）持 RLock，
// 可多读并发；写路径（Set + handleOwnerBind 的 check-and-write）持 Lock，独占。
// handleOwnerBind 的 check + write + persist + revoke-code 整段在写锁内，原子完成。
type ownerBindingStore struct {
	mu       sync.RWMutex
	dir      string
	bindings map[string]ownerBinding // entrypointID → binding
}

// newOwnerBindingStore 创建一个 store，并从 dir 加载已持久化的绑定关系。
// 文件不存在或非法时返回空 store（不报错——绑定关系丢失不阻止 agent 启动）。
func newOwnerBindingStore(dir string) *ownerBindingStore {
	s := &ownerBindingStore{
		dir:      dir,
		bindings: make(map[string]ownerBinding),
	}
	s.load()
	return s
}

func (s *ownerBindingStore) path() string {
	return filepath.Join(s.dir, ownerBindingsFilename)
}

// load 从磁盘加载绑定关系到内存。文件不存在或非法 JSON 时静默返回空（带 slog 警告）。
// 调用方：仅 NewService 启动路径，单线程，无需持锁。
func (s *ownerBindingStore) load() {
	data, err := os.ReadFile(s.path())
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("owner_bind: read bindings file failed, starting with empty bindings",
				"path", s.path(), "err", err)
		}
		return
	}
	var f ownerBindingsFile
	if err := json.Unmarshal(data, &f); err != nil {
		slog.Warn("owner_bind: parse bindings file failed, starting with empty bindings",
			"path", s.path(), "err", err)
		return
	}
	for _, b := range f.Bindings {
		s.bindings[b.EntrypointID] = b
	}
}

// Get 返回 entrypointID 对应的绑定。线程安全（RLock，可并发读）。
//
// coverage: contract (100% required)
func (s *ownerBindingStore) Get(entrypointID string) (ownerBinding, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.bindings[entrypointID]
	return b, ok
}

// IsBound 报告该 entrypoint 是否已绑定 owner。线程安全（RLock）。
//
// coverage: contract (100% required)
func (s *ownerBindingStore) IsBound(entrypointID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.bindings[entrypointID]
	return ok
}

// Set 写入一条绑定关系并立即持久化。线程安全（自己加写锁）。
// 注意：handleOwnerBind 不调用此方法（它在自己的写锁里直接操作 map + persistLocked，
// 避免 Lock 重入死锁）。
// coverage: contract (100% required)
func (s *ownerBindingStore) Set(b ownerBinding) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindings[b.EntrypointID] = b
	if err := s.persistLocked(); err != nil {
		// 持久化失败仅记日志——内存绑定已生效（当前进程内 IsOwner 仍正确），
		// 重启会丢失。这是「降级」而非「崩溃」，因为 owner 绑定阻塞启动代价过高。
		slog.Error("owner_bind: persist bindings failed (in-memory binding still active until restart)",
			"err", err)
	}
}

// persistLocked 把内存里的绑定关系写入磁盘。调用方必须持 s.mu。
func (s *ownerBindingStore) persistLocked() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.dir, err)
	}
	f := ownerBindingsFile{Bindings: make([]ownerBinding, 0, len(s.bindings))}
	for _, b := range s.bindings {
		f.Bindings = append(f.Bindings, b)
	}
	// 按 entrypointID 排序，保证输出稳定（避免 map 迭代序导致 diff 噪音）。
	sortOwnerBindings(f.Bindings)
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(s.path(), data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", s.path(), err)
	}
	return nil
}

// setLockedForTest 仅用于测试：直接设内存状态而不持久化（测试持久化时再手动调 persistLocked）。
func (s *ownerBindingStore) setLockedForTest(b ownerBinding) {
	s.bindings[b.EntrypointID] = b
}

func sortOwnerBindings(bs []ownerBinding) {
	// inline sort.Slice 避免导入 sort 包（本文件其它部分不用）
	for i := 1; i < len(bs); i++ {
		for j := i; j > 0 && bs[j-1].EntrypointID > bs[j].EntrypointID; j-- {
			bs[j-1], bs[j] = bs[j], bs[j-1]
		}
	}
}

// handleOwnerBind 处理 "/bind <code>" 指令：验证 code，把 senderID 绑定为 entrypointID 的 owner。
// 返回面向用户的中文提示消息（成功/失败/幂等）。
//
// check + write + persist + revoke-code 整段持 ownerBindings.mu 原子完成——
// 保证并发 /bind 不会写花（silent data corruption 防御，AGENTS.md §2 重灾区）。
//
// 检查顺序（重要）：先 IsBound 再 configured。原因：在新模型下 code 绑定成功后立即作废
// （delete from bindCodes），所以「configured=false」通常意味着「已绑定」。先查 IsBound
// 能给已绑场景正确的「你已经是主人 / 已有主人」提示，而不是误导性的「未启用绑定」。
//
// coverage: contract (100% required)
func (s *Service) handleOwnerBind(entrypointID, senderID, code string) string {
	s.ownerBindings.mu.Lock()
	defer s.ownerBindings.mu.Unlock()

	// ① 已绑定？
	if existing, bound := s.ownerBindings.bindings[entrypointID]; bound {
		if existing.OwnerSenderID == senderID {
			return "✅ 你已经是 " + entrypointID + " 的主人了，无需重复绑定。"
		}
		return "❌ 该入口（" + entrypointID + "）已有主人，无法重新绑定。"
	}

	// ② 配置了绑定码？（未绑定 + 未配置 = 真的未启用）
	expected, configured := s.readBindCode(entrypointID)
	if !configured {
		return "❌ 入口 " + entrypointID + " 未启用绑定（启动时未生成绑定码）。"
	}

	// ③ code 匹配？（ConstantTimeCompare 防 timing 侧信道）
	if subtle.ConstantTimeCompare([]byte(code), []byte(expected)) != 1 {
		return "❌ 绑定码无效，请核对后重试。"
	}

	// ④ 写入绑定 + 持久化。持久化失败 = 绑定未真正建立（重启会丢），所以
	// 回滚内存写入、不作废 code、返回失败——「成功」语义诚实等于已落盘。
	binding := ownerBinding{
		EntrypointID:  entrypointID,
		OwnerSenderID: senderID,
		BoundAt:       time.Now().UTC(),
	}
	s.ownerBindings.bindings[entrypointID] = binding
	if err := s.ownerBindings.persistLocked(); err != nil {
		delete(s.ownerBindings.bindings, entrypointID) // 回滚内存写入
		slog.Error("owner_bind: persist binding failed, binding rolled back",
			"entrypoint_id", entrypointID, "err", err)
		return "❌ 绑定失败：无法保存绑定关系，请检查系统配置后重试。"
	}
	s.revokeBindCode(entrypointID) // 持久化成功才作废 code

	return "✅ 绑定成功。你现在是 " + entrypointID + " 的主人。"
}

// readBindCode 读出某 entrypoint 的绑定码（线程安全）。
func (s *Service) readBindCode(entrypointID string) (string, bool) {
	s.bindCodesMu.Lock()
	defer s.bindCodesMu.Unlock()
	code, ok := s.bindCodes[entrypointID]
	return code, ok
}

// revokeBindCode 作废某 entrypoint 的绑定码（绑定成功后调用，线程安全）。
func (s *Service) revokeBindCode(entrypointID string) {
	s.bindCodesMu.Lock()
	defer s.bindCodesMu.Unlock()
	delete(s.bindCodes, entrypointID)
}

// generateAndAnnounceBindCodes 为每个尚未绑定 owner 的 entrypoint 生成一次性 device code，
// 并打印到 stdout。已绑定的 entrypoint 不生成（不会出现在绑定码列表里）。
// 在 NewService 末尾调用（启动时一次）。
func (s *Service) generateAndAnnounceBindCodes() {
	if s.entrypoints == nil || s.ownerBindings == nil {
		return
	}
	var lines []string
	for _, def := range s.entrypoints.Definitions() {
		if s.ownerBindings.IsBound(def.ID) {
			continue // 已绑定，不生成 code
		}
		code := generateBindCode()
		s.bindCodes[def.ID] = code
		lines = append(lines, fmt.Sprintf("  %s  → 在 IM 发: /bind %s", def.ID, code))
	}
	if len(lines) == 0 {
		return
	}
	// 打印到 stdout（不是 slog）——一次性 device code 不应被结构化日志收集系统归档。
	// 对齐 GitHub device flow 的做法（显示在终端给人看，用完即焚）。
	fmt.Println("\nowner 绑定（未绑定 owner 的 entrypoint）：")
	for _, l := range lines {
		fmt.Println(l)
	}
	fmt.Println("\n绑定成功后此码作废。重启会生成新码（已绑定的不再生成）。")
}

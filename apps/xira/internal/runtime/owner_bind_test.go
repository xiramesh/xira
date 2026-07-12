package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/channel"
)

// parseBindCommand 识别 "/bind <token>" 指令。

func TestParseBindCommand(t *testing.T) {
	cases := []struct {
		name    string
		msg     string
		wantTok string
		wantOK  bool
	}{
		{"normal", "/bind WDJM-LHKD", "WDJM-LHKD", true},
		{"multiple spaces", "/bind   WDJM-LHKD", "WDJM-LHKD", true},
		{"leading/trailing whitespace", "  /bind WDJM-LHKD\n", "WDJM-LHKD", true},
		{"trailing text takes first token", "/bind WDJM-LHKD extra", "WDJM-LHKD", true},
		{"newline separated", "/bind\nWDJM-LHKD", "WDJM-LHKD", true},
		{"no arg -> passthrough", "/bind", "", false},
		{"no arg with trailing space -> passthrough", "/bind ", "", false},
		{"natural language -> passthrough", "帮我 /bind 一下", "", false},
		{"empty string", "", "", false},
		{"plain text", "hello world", "", false},
		{"bind substring not prefix", "请/bind WDJM-LHKD", "", false},
		{"binder not bind", "/binder WDJM-LHKD", "", false},
		{"bind no space then text", "/bind/bind", "", false},
		{"case sensitive", "/Bind WDJM-LHKD", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotTok, gotOK := parseBindCommand(tc.msg)
			if gotTok != tc.wantTok || gotOK != tc.wantOK {
				t.Errorf("parseBindCommand(%q) = (%q, %v), want (%q, %v)",
					tc.msg, gotTok, gotOK, tc.wantTok, tc.wantOK)
			}
		})
	}
}

// generateBindCode 生成 8 字符 base32 码（去易混字符），形如 "WDJM-LHKD"。

func TestGenerateBindCode(t *testing.T) {
	code := generateBindCode()

	t.Run("length is 9 (8 chars + 1 dash)", func(t *testing.T) {
		if len(code) != 9 {
			t.Fatalf("len(generateBindCode()) = %d, want 9 (got %q)", len(code), code)
		}
	})

	t.Run("dash at position 4", func(t *testing.T) {
		if code[4] != '-' {
			t.Errorf("code[4] = %q, want '-' (got %q)", code[4], code)
		}
	})

	t.Run("no ambiguous chars", func(t *testing.T) {
		ambiguous := "01OI"
		for _, r := range code {
			if strings.ContainsRune(ambiguous, r) {
				t.Errorf("code %q contains ambiguous char %q", code, r)
			}
		}
	})

	t.Run("all chars in alphabet", func(t *testing.T) {
		for _, r := range code {
			if r == '-' {
				continue
			}
			if !strings.ContainsRune(bindCodeAlphabet, r) {
				t.Errorf("code %q contains char %q not in alphabet %q", code, r, bindCodeAlphabet)
			}
		}
	})

	t.Run("two calls produce different codes", func(t *testing.T) {
		code2 := generateBindCode()
		if code == code2 {
			t.Errorf("two consecutive generateBindCode() calls both = %q, want different", code)
		}
	})

	t.Run("many calls no duplicates", func(t *testing.T) {
		seen := make(map[string]bool, 1000)
		for i := 0; i < 1000; i++ {
			c := generateBindCode()
			if seen[c] {
				t.Fatalf("duplicate code %q after %d generations", c, i)
			}
			seen[c] = true
		}
	})
}

// IsBindCommand 是 runner pre-auth 放行的判定（导出，跨包用）。

func TestIsBindCommand(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"/bind WDJM-LHKD", true},
		{"/bind   WDJM-LHKD", true},
		{"  /bind WDJM-LHKD\n", true},
		{"/bind", false}, // 无参不算
		{"hello", false},
		{"", false},
		{"/binder X", false},
	}
	for _, tc := range cases {
		if got := IsBindCommand(tc.msg); got != tc.want {
			t.Errorf("IsBindCommand(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// ownerBindingStore 持久化 owner 绑定关系到 <stateDir>/owner-bindings.json。

func TestNewOwnerBindingStore_LoadsExistingBindings(t *testing.T) {
	dir := t.TempDir()
	// 先用一个 store 写入一条绑定
	s1 := newOwnerBindingStore(dir)
	s1.setLockedForTest(ownerBinding{
		EntrypointID:      "feishu-default",
		OwnerSenderID:     "ou_owner",
		OwnerSenderIDType: "open_id",
		BoundAt:           time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC),
	})
	if err := s1.persistLocked(); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// 新建 store 从同目录加载 → 应读到已持久化的绑定
	s2 := newOwnerBindingStore(dir)
	got, ok := s2.Get("feishu-default")
	if !ok {
		t.Fatal("expected binding for feishu-default after reload")
	}
	if got.OwnerSenderID != "ou_owner" {
		t.Errorf("OwnerSenderID = %q, want ou_owner", got.OwnerSenderID)
	}
	if got.OwnerSenderIDType != "open_id" {
		t.Errorf("OwnerSenderIDType = %q, want open_id", got.OwnerSenderIDType)
	}
	if got.EntrypointID != "feishu-default" {
		t.Errorf("EntrypointID = %q, want feishu-default", got.EntrypointID)
	}
	if got.BoundAt.IsZero() {
		t.Error("BoundAt is zero, want persisted time")
	}
}

func TestNewOwnerBindingStore_EmptyWhenNoFile(t *testing.T) {
	dir := t.TempDir()
	s := newOwnerBindingStore(dir)
	if _, ok := s.Get("anything"); ok {
		t.Error("expected no binding when owner-bindings.json does not exist")
	}
	if bound := s.IsBound("anything"); bound {
		t.Error("IsBound = true, want false for empty store")
	}
}

func TestNewOwnerBindingStore_EmptyWhenMalformedFile(t *testing.T) {
	dir := t.TempDir()
	// 写一个非法 JSON 的文件 → load 应容忍（返回空，不 panic）
	if err := os.WriteFile(filepath.Join(dir, ownerBindingsFilename), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newOwnerBindingStore(dir)
	if bound := s.IsBound("feishu-default"); bound {
		t.Error("IsBound = true for malformed file, want false")
	}
}

func TestOwnerBindingStore_SetAndPersist(t *testing.T) {
	dir := t.TempDir()
	s := newOwnerBindingStore(dir)

	s.Set(ownerBinding{
		EntrypointID:  "feishu-default",
		OwnerSenderID: "ou_owner",
		BoundAt:       time.Now().UTC(),
	})

	// 内存可见
	got, ok := s.Get("feishu-default")
	if !ok || got.OwnerSenderID != "ou_owner" {
		t.Fatalf("after Set, Get = (%+v, %v), want ou_owner", got, ok)
	}
	// 持久化文件存在
	data, err := os.ReadFile(filepath.Join(dir, ownerBindingsFilename))
	if err != nil {
		t.Fatalf("read persisted file: %v", err)
	}
	if !strings.Contains(string(data), "ou_owner") {
		t.Errorf("persisted file %s does not contain ou_owner", data)
	}
}

func TestOwnerBindingStore_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	s := newOwnerBindingStore(dir)
	s.Set(ownerBinding{
		EntrypointID:  "feishu-default",
		OwnerSenderID: "ou_owner",
		BoundAt:       time.Now().UTC(),
	})

	info, err := os.Stat(filepath.Join(dir, ownerBindingsFilename))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0o600 {
		t.Errorf("file perm = %o, want 0600", perm)
	}
}

func TestOwnerBindingStore_IsBound(t *testing.T) {
	dir := t.TempDir()
	s := newOwnerBindingStore(dir)
	s.Set(ownerBinding{EntrypointID: "ep-a", OwnerSenderID: "u1", BoundAt: time.Now()})

	if !s.IsBound("ep-a") {
		t.Error("IsBound(ep-a) = false, want true")
	}
	if s.IsBound("ep-b") {
		t.Error("IsBound(ep-b) = true, want false")
	}
}

// newBindTestService 构造一个仅含 owner 绑定所需字段的最小 *Service，用于 handleOwnerBind 单测。
func newBindTestService(t *testing.T) *Service {
	t.Helper()
	svc := &Service{
		ownerBindings: newOwnerBindingStore(t.TempDir()),
		bindCodes:     map[string]string{},
	}
	return svc
}

// handleOwnerBind 是 owner 绑定的核心逻辑。

func TestHandleOwnerBind_HappyPath(t *testing.T) {
	svc := newBindTestService(t)
	svc.bindCodes["feishu-default"] = "WDJM-LHKD"

	msg := svc.handleOwnerBind("feishu-default", "ou_owner", "WDJM-LHKD")

	if !strings.Contains(msg, "绑定成功") {
		t.Errorf("msg = %q, want contains 绑定成功", msg)
	}
	if !svc.ownerBindings.IsBound("feishu-default") {
		t.Error("after happy bind, IsBound = false, want true")
	}
	binding, _ := svc.ownerBindings.Get("feishu-default")
	if binding.OwnerSenderID != "ou_owner" {
		t.Errorf("OwnerSenderID = %q, want ou_owner", binding.OwnerSenderID)
	}
	// code 被作废
	if _, stillThere := svc.bindCodes["feishu-default"]; stillThere {
		t.Error("bind code not revoked after successful bind")
	}
}

func TestHandleOwnerBind_WrongCodeDoesNotConsume(t *testing.T) {
	svc := newBindTestService(t)
	svc.bindCodes["feishu-default"] = "WDJM-LHKD"

	msg := svc.handleOwnerBind("feishu-default", "ou_owner", "WRONG-CODE")

	if !strings.Contains(msg, "无效") {
		t.Errorf("msg = %q, want contains 无效", msg)
	}
	// code 不作废（可重试）
	if _, stillThere := svc.bindCodes["feishu-default"]; !stillThere {
		t.Error("bind code was revoked on wrong attempt, should remain for retry")
	}
	// 不写绑定
	if svc.ownerBindings.IsBound("feishu-default") {
		t.Error("IsBound = true after wrong code, want false")
	}
}

func TestHandleOwnerBind_IdempotentSameSender(t *testing.T) {
	svc := newBindTestService(t)
	svc.bindCodes["feishu-default"] = "WDJM-LHKD"

	// 预置已绑定（同 sender）——模拟先成功一次
	first := svc.handleOwnerBind("feishu-default", "ou_owner", "WDJM-LHKD")
	if !strings.Contains(first, "绑定成功") {
		t.Fatalf("first bind msg = %q", first)
	}
	// 第二次同 sender（code 已作废，但 IsBound 命中应走幂等分支）
	second := svc.handleOwnerBind("feishu-default", "ou_owner", "anything")
	if !strings.Contains(second, "已经是") {
		t.Errorf("idempotent msg = %q, want contains 已经是", second)
	}
}

func TestHandleOwnerBindWithIdentityEnrichesLegacyBinding(t *testing.T) {
	svc := newBindTestService(t)
	svc.ownerBindings.Set(ownerBinding{
		EntrypointID:  "feishu-default",
		OwnerSenderID: "ou_owner",
		BoundAt:       time.Now().UTC(),
	})

	msg := svc.handleOwnerBindWithIdentity("feishu-default", "ou_owner", "open_id", "unused")
	if !strings.Contains(msg, "已经是") {
		t.Fatalf("msg = %q, want idempotent already-owner response", msg)
	}
	got, ok := svc.ownerBindings.Get("feishu-default")
	if !ok || got.OwnerSenderIDType != "open_id" {
		t.Fatalf("legacy binding not enriched: %+v, ok=%v", got, ok)
	}

	reloaded := newOwnerBindingStore(svc.ownerBindings.dir)
	persisted, ok := reloaded.Get("feishu-default")
	if !ok || persisted.OwnerSenderIDType != "open_id" {
		t.Fatalf("enriched type not persisted: %+v, ok=%v", persisted, ok)
	}
}

func TestHandleOwnerBind_RejectsImpersonator(t *testing.T) {
	svc := newBindTestService(t)
	svc.bindCodes["feishu-default"] = "WDJM-LHKD"

	// 真 owner 先绑
	svc.handleOwnerBind("feishu-default", "ou_owner", "WDJM-LHKD")
	// 冒领者（不同 sender）尝试
	msg := svc.handleOwnerBind("feishu-default", "ou_attacker", "WDJM-LHKD")

	if !strings.Contains(msg, "已有主人") {
		t.Errorf("impersonator msg = %q, want contains 已有主人", msg)
	}
	// owner 不变
	binding, _ := svc.ownerBindings.Get("feishu-default")
	if binding.OwnerSenderID != "ou_owner" {
		t.Errorf("OwnerSenderID = %q, want ou_owner (not overwritten by attacker)", binding.OwnerSenderID)
	}
}

func TestHandleOwnerBind_NotConfigured(t *testing.T) {
	// entrypoint 既没配 code 也没绑定 → 未启用
	svc := newBindTestService(t)
	msg := svc.handleOwnerBind("feishu-unconfigured", "ou_owner", "ANY-CODE")

	if !strings.Contains(msg, "未启用") {
		t.Errorf("msg = %q, want contains 未启用", msg)
	}
	if svc.ownerBindings.IsBound("feishu-unconfigured") {
		t.Error("IsBound = true for unconfigured entrypoint")
	}
}

func TestHandleOwnerBind_Concurrent(t *testing.T) {
	// N 个 goroutine 同时 /bind 同一个 code → 恰好 1 个成功 + N-1 个失败
	// （silent data corruption 防御，AGENTS.md §2 重灾区）
	const N = 50
	svc := newBindTestService(t)
	svc.bindCodes["feishu-default"] = "WDJM-LHKD"

	var wg sync.WaitGroup
	successCount := int64(0)
	results := make([]string, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			// 每个 goroutine 用不同 sender，确保只有第一个能成功，其余应被「已有主人」拦
			senderID := fmt.Sprintf("ou_%d", i)
			results[i] = svc.handleOwnerBind("feishu-default", senderID, "WDJM-LHKD")
			if strings.Contains(results[i], "绑定成功") {
				atomic.AddInt64(&successCount, 1)
			}
		}()
	}
	wg.Wait()

	if successCount != 1 {
		t.Errorf("concurrent bind: successCount = %d, want exactly 1", successCount)
		// 打印所有结果帮助诊断
		for i, r := range results {
			t.Logf("  goroutine %d: %q", i, r)
		}
	}
	// bindings 文件恰好 1 条
	bindings := svc.ownerBindings.bindings
	if len(bindings) != 1 {
		t.Errorf("bindings map len = %d, want 1", len(bindings))
	}
	// code 已作废
	if _, stillThere := svc.bindCodes["feishu-default"]; stillThere {
		t.Error("bind code not revoked after concurrent bind")
	}
}

// RunAgent 拦截 /bind：命中走绑定，不进 agent turn。

func newBindRunAgentService(t *testing.T) (*Service, string) {
	t.Helper()
	instance := writeRuntimeFixture(t, "xira-assistant", []string{"chat", "sender"})
	writeFile(t, filepath.Join(instance, "xira.yaml"), `workspace: workspace
default_agent: xira-assistant
entrypoints: entrypoints.yaml
`)
	writeFile(t, filepath.Join(instance, "workspace", "entrypoints.yaml"), `entrypoints:
  - id: feishu-default
    channel: feishu
    default_agent: xira-assistant
`)
	rt := newTestService(t, Config{ConfigPath: filepath.Join(instance, "xira.yaml")})
	// newTestService 已为 feishu-default 生成一个 code，取出来用。
	code := rt.bindCodes["feishu-default"]
	if code == "" {
		t.Fatal("expected bind code generated for feishu-default")
	}
	return rt, code
}

func TestRunAgent_InterceptsBindCommand_Success(t *testing.T) {
	rt, code := newBindRunAgentService(t)

	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "/bind " + code,
		Context: channel.InboundContext{
			Channel:  "feishu",
			SenderID: "ou_owner",
			ChatID:   "chat-1",
			ChatType: "p2p",
		},
		EntrypointID: "feishu-default",
	})

	if err != nil {
		t.Fatalf("RunAgent error: %v", err)
	}
	if !strings.Contains(resp.FinalResponse, "绑定成功") {
		t.Errorf("FinalResponse = %q, want contains 绑定成功", resp.FinalResponse)
	}
	if resp.Status != "completed" {
		t.Errorf("Status = %q, want completed", resp.Status)
	}
	// 绑定生效
	if !rt.IsOwner(context.Background(), "ou_owner", "feishu-default") {
		t.Error("after /bind, IsOwner(ou_owner) = false, want true")
	}
	// code 作废
	if _, stillThere := rt.bindCodes["feishu-default"]; stillThere {
		t.Error("bind code not revoked after /bind via RunAgent")
	}
	// 无副作用：不产生 run 记录（runs 目录应为空）
	matches, _ := filepath.Glob(filepath.Join(rt.stateDir, "runs", "*"))
	if len(matches) > 0 {
		t.Errorf("expected no run records, found %d: %v", len(matches), matches)
	}
}

func TestRunAgent_InterceptsBindCommand_WrongCode(t *testing.T) {
	rt, _ := newBindRunAgentService(t)

	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "/bind WRONG-CODE",
		Context: channel.InboundContext{
			Channel: "feishu", SenderID: "ou_owner", ChatID: "chat-1", ChatType: "p2p",
		},
		EntrypointID: "feishu-default",
	})

	if err != nil {
		t.Fatalf("RunAgent error: %v", err)
	}
	if !strings.Contains(resp.FinalResponse, "无效") {
		t.Errorf("FinalResponse = %q, want contains 无效", resp.FinalResponse)
	}
	// 不写绑定
	if rt.IsOwner(context.Background(), "ou_owner", "feishu-default") {
		t.Error("IsOwner = true after wrong code, want false")
	}
}

func TestRunAgent_PassthroughNormalMessage(t *testing.T) {
	// 普通消息不应被 /bind 拦截，应正常进 agent turn。
	rt, _ := newBindRunAgentService(t)

	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "你好",
		Context: channel.InboundContext{
			Channel: "feishu", SenderID: "ou_user", ChatID: "chat-1", ChatType: "p2p",
		},
		EntrypointID: "feishu-default",
	})

	if err != nil {
		t.Fatalf("RunAgent error: %v", err)
	}
	// 正常 turn 应该有 Status（completed/failed 等），且 FinalResponse 不是绑定提示。
	if resp.Status == "" {
		t.Error("normal message: Status empty, expected a real turn to run")
	}
	if strings.Contains(resp.FinalResponse, "绑定") {
		t.Errorf("normal message was intercepted as bind: %q", resp.FinalResponse)
	}
	// 不该建立任何 owner 绑定
	if rt.IsOwner(context.Background(), "ou_user", "feishu-default") {
		t.Error("normal message should not create owner binding")
	}
}

func TestRunAgent_PassthroughBindNoArg(t *testing.T) {
	// "/bind"（无参）应放行进 agent turn（让 agent 解释绑定用法）。
	rt, _ := newBindRunAgentService(t)

	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "/bind",
		Context: channel.InboundContext{
			Channel: "feishu", SenderID: "ou_user", ChatID: "chat-1", ChatType: "p2p",
		},
		EntrypointID: "feishu-default",
	})

	if err != nil {
		t.Fatalf("RunAgent error: %v", err)
	}
	if strings.Contains(resp.FinalResponse, "绑定成功") {
		t.Errorf("/bind (no arg) was treated as bind attempt: %q", resp.FinalResponse)
	}
}

// TestIsOwnerConcurrentWithBind 验证 IsOwner（授权热路径，并发读）和 /bind（写）
// 不会发生 data race。必须用 -race 跑：go test -race -run TestIsOwnerConcurrentWithBind。
//
// 这是 reviewer 复现的 blocker：Get/IsBound 直接读 bindings map 无锁，
// handleOwnerBind 写同一个 map → concurrent map read and map write。
func TestIsOwnerConcurrentWithBind(t *testing.T) {
	svc := newBindTestService(t)
	svc.bindCodes["ep-race"] = "RACE-CODE"
	ctx := context.Background()

	var wg sync.WaitGroup
	// 一组 goroutine 持续 /bind 写（写入不同 entrypoint 触发 map grow）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			ep := fmt.Sprintf("ep-%d", i%5)
			svc.bindCodes[ep] = fmt.Sprintf("CODE-%d", i)
			svc.handleOwnerBind(ep, fmt.Sprintf("ou_%d", i), fmt.Sprintf("CODE-%d", i))
		}
	}()
	// 一组 goroutine 持续 IsOwner 读（授权热路径）
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				// 不断读各种 entrypoint 的 owner —— 和写并发
				svc.IsOwner(ctx, "ou_owner", fmt.Sprintf("ep-%d", i%5))
			}
		}()
	}
	wg.Wait()
}

// --- 覆盖率补充：错误路径 / 边界分支（§5.2 契约函数 100%）---

func TestOwnerBindingStore_PersistMultipleSortsByID(t *testing.T) {
	// 多条 binding → persistLocked 走排序分支（sortOwnerBindings 内层循环）。
	dir := t.TempDir()
	s := newOwnerBindingStore(dir)
	s.Set(ownerBinding{EntrypointID: "zebra", OwnerSenderID: "u1", BoundAt: time.Now()})
	s.Set(ownerBinding{EntrypointID: "alpha", OwnerSenderID: "u2", BoundAt: time.Now()})
	s.Set(ownerBinding{EntrypointID: "mike", OwnerSenderID: "u3", BoundAt: time.Now()})

	// reload 后顺序应为 alpha < mike < zebra
	s2 := newOwnerBindingStore(dir)
	data, _ := os.ReadFile(filepath.Join(dir, ownerBindingsFilename))
	body := string(data)
	alphaPos := strings.Index(body, "alpha")
	mikePos := strings.Index(body, "mike")
	zebraPos := strings.Index(body, "zebra")
	if !(alphaPos < mikePos && mikePos < zebraPos) {
		t.Errorf("bindings not sorted by entrypoint_id in file:\n%s", body)
	}
	if _, ok := s2.Get("zebra"); !ok {
		t.Error("reload missing zebra")
	}
}

func TestOwnerBindingStore_PersistFailsGracefully(t *testing.T) {
	// Set 在持久化失败时不应 panic（降级：内存绑定生效，仅记日志）。
	// 用一个不可写的目录模拟失败（文件路径指向一个已存在文件的子路径）。
	dir := t.TempDir()
	// 创建一个文件占位，使 dir/owner-bindings.json 的父目录其实是个文件 → MkdirAll 失败。
	blocker := filepath.Join(dir, ownerBindingsFilename)
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 用一个把 owner-bindings.json 路径挤进文件子路径的 store（手工构造）。
	s := &ownerBindingStore{
		dir:      filepath.Join(dir, ownerBindingsFilename), // 父路径是个文件 → MkdirAll 失败
		bindings: map[string]ownerBinding{},
	}
	// Set 不应 panic，内存状态仍生效。
	s.Set(ownerBinding{EntrypointID: "ep", OwnerSenderID: "u", BoundAt: time.Now()})
	if !s.IsBound("ep") {
		t.Error("Set did not update in-memory state on persist failure")
	}
}

func TestOwnerBindingStore_LoadReadErrorNonNotExist(t *testing.T) {
	// load 遇到「存在但读不了」的文件（如权限不足）应走非 IsNotExist 分支，静默返回空。
	// 这里用一个目录占位文件名模拟 ReadFile 失败。
	dir := t.TempDir()
	target := filepath.Join(dir, ownerBindingsFilename)
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err) // target 现在是目录 → ReadFile 会失败（不是 IsNotExist）
	}
	s := newOwnerBindingStore(dir)
	if s.IsBound("anything") {
		t.Error("expected empty store when bindings path is a directory (read error)")
	}
}

func TestGenerateAndAnnounceBindCodes_NilGuards(t *testing.T) {
	// entrypoints 或 ownerBindings 为 nil 时不 panic（防御 nil Service）。
	s := &Service{}
	s.generateAndAnnounceBindCodes() // 不应 panic

	s2 := &Service{ownerBindings: newOwnerBindingStore(t.TempDir())}
	s2.generateAndAnnounceBindCodes() // entrypoints 仍 nil，不应 panic
}

func TestGenerateAndAnnounceBindCodes_SkipsBoundEntrypoints(t *testing.T) {
	// 已绑定的 entrypoint 不生成 code（generateAndAnnounceBindCodes 的 IsBound 跳过分支）。
	instance := writeRuntimeFixture(t, "xira-assistant", []string{"chat", "sender"})
	writeFile(t, filepath.Join(instance, "xira.yaml"), `workspace: workspace
default_agent: xira-assistant
entrypoints: entrypoints.yaml
`)
	writeFile(t, filepath.Join(instance, "workspace", "entrypoints.yaml"), `entrypoints:
  - id: ep-bound
    channel: feishu
    default_agent: xira-assistant
  - id: ep-free
    channel: feishu
    default_agent: xira-assistant
`)
	rt := newTestService(t, Config{ConfigPath: filepath.Join(instance, "xira.yaml")})

	// 先绑定 ep-bound
	boundCode := rt.bindCodes["ep-bound"]
	rt.handleOwnerBind("ep-bound", "ou_owner", boundCode)
	// 绑定后 ep-bound 的 code 应已被作废（handleOwnerBind 里 delete）
	if _, hasBound := rt.bindCodes["ep-bound"]; hasBound {
		t.Fatal("ep-bound code should be revoked after bind")
	}

	// 再次跑生成逻辑：ep-bound 已绑 → 不应重新生成 code；ep-free 仍有 code。
	rt.generateAndAnnounceBindCodes()
	if _, hasBound := rt.bindCodes["ep-bound"]; hasBound {
		t.Error("ep-bound should not get a new code after it's bound")
	}
	if _, hasFree := rt.bindCodes["ep-free"]; !hasFree {
		t.Error("ep-free should still have a code")
	}
}

func TestHandleOwnerBind_PersistFailureRollsBackAndKeepsCode(t *testing.T) {
	// handleOwnerBind 在 persistLocked 失败时应回滚内存写入、不作废 code、返回失败。
	// 「成功」语义诚实等于已落盘（reviewer non-block 建议）。
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ownerBindingsFilename), 0o700) // 让 owner-bindings.json 路径变成目录
	svc := &Service{
		ownerBindings: &ownerBindingStore{
			dir:      dir,
			bindings: map[string]ownerBinding{},
		},
		bindCodes: map[string]string{"ep": "CODE-1234"},
	}
	msg := svc.handleOwnerBind("ep", "ou_owner", "CODE-1234")
	// 持久化失败 → 不返回成功。
	if strings.Contains(msg, "绑定成功") {
		t.Errorf("msg = %q, should NOT report success on persist failure", msg)
	}
	if !strings.Contains(msg, "绑定失败") {
		t.Errorf("msg = %q, want contains 绑定失败", msg)
	}
	// 内存写入被回滚。
	if svc.ownerBindings.IsBound("ep") {
		t.Error("binding should be rolled back from memory on persist failure")
	}
	// code 不作废（可重试）。
	if _, stillThere := svc.bindCodes["ep"]; !stillThere {
		t.Error("bind code should NOT be revoked on persist failure (must allow retry)")
	}
}

func TestHandleOwnerBindWithIdentity_EnrichmentPersistFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ownerBindingsFilename), 0o700)
	legacy := ownerBinding{EntrypointID: "ep", OwnerSenderID: "ou_owner", BoundAt: time.Now().UTC()}
	svc := &Service{
		ownerBindings: &ownerBindingStore{
			dir:      dir,
			bindings: map[string]ownerBinding{"ep": legacy},
		},
		bindCodes: map[string]string{},
	}

	msg := svc.handleOwnerBindWithIdentity("ep", "ou_owner", "open_id", "unused")
	if !strings.Contains(msg, "无法保存") {
		t.Fatalf("msg = %q, want persistence failure", msg)
	}
	got, ok := svc.ownerBindings.Get("ep")
	if !ok || got.OwnerSenderIDType != "" {
		t.Fatalf("failed enrichment must roll back: %+v, ok=%v", got, ok)
	}
}

func TestIsOwner_NilGuards(t *testing.T) {
	// IsOwner 各 nil/空 分支。
	var nilSvc *Service
	if nilSvc.IsOwner(context.Background(), "u", "ep") {
		t.Error("nil Service.IsOwner should be false")
	}
	svc := &Service{} // ownerBindings nil, entrypoints nil
	if svc.IsOwner(context.Background(), "u", "ep") {
		t.Error("empty Service.IsOwner should be false")
	}
	if svc.IsOwner(context.Background(), "", "ep") {
		t.Error("empty sender should be false")
	}
	if svc.IsOwner(context.Background(), "u", "") {
		t.Error("empty entrypoint should be false")
	}
	// ownerBindings 非 nil 但无该 entrypoint，entrypoints 也 nil → fallback 路径返回 false。
	svc2 := &Service{ownerBindings: newOwnerBindingStore(t.TempDir())}
	if svc2.IsOwner(context.Background(), "u", "ep") {
		t.Error("no binding + nil entrypoints should be false")
	}
}

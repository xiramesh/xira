package cron

import (
	"testing"
)

// principalHashGolden 是一个固定的 golden value，用于确保 PrincipalHash 的 versioned
// 前缀和规范化规则不被无意修改——改规范化必须 bump PrincipalHashVersion。
//
// 计算方式：sha256("cron-principal-v1\x00feishu-main\x00feishu\x00feishu_open_id\x00ou_yinwm")
// 把这串打印出来填到这里。
const principalHashGolden = "REPLACE_WITH_REAL_HASH"

func TestPrincipalNormalize(t *testing.T) {
	tests := []struct {
		name  string
		input CronPrincipal
		want  CronPrincipal
	}{
		{
			name: "trim 全字段",
			input: CronPrincipal{
				EntrypointID: "  feishu-main  ",
				Channel:      "  feishu  ",
				SenderIDType: "  feishu_open_id  ",
				SenderID:     "  ou_yinwm  ",
			},
			want: CronPrincipal{
				EntrypointID: "feishu-main",
				Channel:      "feishu",
				SenderIDType: "feishu_open_id",
				SenderID:     "ou_yinwm",
			},
		},
		{
			name: "channel 和 type 转 lowercase，entrypoint 和 sender 保留大小写",
			input: CronPrincipal{
				EntrypointID: "Feishu-Main",
				Channel:      "FEISHU",
				SenderIDType: "Feishu_Open_ID",
				SenderID:     "Ou_Yinwm",
			},
			want: CronPrincipal{
				EntrypointID: "Feishu-Main",
				Channel:      "feishu",
				SenderIDType: "feishu_open_id",
				SenderID:     "Ou_Yinwm",
			},
		},
		{
			name: "空字段 trim 到空字符串",
			input: CronPrincipal{
				EntrypointID: "   ",
				Channel:      "feishu",
				SenderIDType: "feishu_open_id",
				SenderID:     "ou_yinwm",
			},
			want: CronPrincipal{
				EntrypointID: "",
				Channel:      "feishu",
				SenderIDType: "feishu_open_id",
				SenderID:     "ou_yinwm",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizePrincipal(tt.input)
			if got != tt.want {
				t.Errorf("NormalizePrincipal = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPrincipalValidate(t *testing.T) {
	tests := []struct {
		name    string
		input   CronPrincipal
		wantErr bool
	}{
		{
			name: "完整 Principal 通过",
			input: CronPrincipal{
				EntrypointID: "feishu-main",
				Channel:      "feishu",
				SenderIDType: "feishu_open_id",
				SenderID:     "ou_yinwm",
			},
			wantErr: false,
		},
		{
			name: "typed identity 为空（SenderIDType 空）拒绝",
			input: CronPrincipal{
				EntrypointID: "feishu-main",
				Channel:      "feishu",
				SenderIDType: "",
				SenderID:     "ou_yinwm",
			},
			wantErr: true,
		},
		{
			name: "typed identity 为空（SenderID 空）拒绝",
			input: CronPrincipal{
				EntrypointID: "feishu-main",
				Channel:      "feishu",
				SenderIDType: "feishu_open_id",
				SenderID:     "",
			},
			wantErr: true,
		},
		{
			name: "typed identity 为空（两者都空）拒绝",
			input: CronPrincipal{
				EntrypointID: "feishu-main",
				Channel:      "feishu",
			},
			wantErr: true,
		},
		{
			name: "entrypoint 空 拒绝",
			input: CronPrincipal{
				Channel:      "feishu",
				SenderIDType: "feishu_open_id",
				SenderID:     "ou_yinwm",
			},
			wantErr: true,
		},
		{
			name: "channel 空 拒绝",
			input: CronPrincipal{
				EntrypointID: "feishu-main",
				SenderIDType: "feishu_open_id",
				SenderID:     "ou_yinwm",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Validate 应在规范化之后做
			err := ValidatePrincipal(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatePrincipal error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPrincipalHash(t *testing.T) {
	t.Run("versioned 前缀和规范化规则 golden value", func(t *testing.T) {
		p := CronPrincipal{
			EntrypointID: "feishu-main",
			Channel:      "feishu",
			SenderIDType: "feishu_open_id",
			SenderID:     "ou_yinwm",
		}
		got := PrincipalHash(p)
		if got != principalHashGolden {
			t.Errorf("PrincipalHash = %q, want golden %q", got, principalHashGolden)
			t.Logf("若规范化规则有意修改，请 bump PrincipalHashVersion 并更新 golden")
		}
	})

	t.Run("规范化后再 hash：channel 和 type 大小写及各字段空格不影响结果", func(t *testing.T) {
		a := CronPrincipal{
			EntrypointID: "  feishu-main  ",
			Channel:      "FEISHU",
			SenderIDType: "Feishu_Open_ID",
			SenderID:     "  ou_yinwm  ",
		}
		b := CronPrincipal{
			EntrypointID: "feishu-main",
			Channel:      "feishu",
			SenderIDType: "feishu_open_id",
			SenderID:     "ou_yinwm",
		}
		if PrincipalHash(a) != PrincipalHash(b) {
			t.Errorf("规范化后 hash 应相等: a=%q b=%q", PrincipalHash(a), PrincipalHash(b))
		}
	})

	t.Run("entrypoint 大小写不同 → hash 不同", func(t *testing.T) {
		lower := CronPrincipal{
			EntrypointID: "feishu-main",
			Channel:      "feishu",
			SenderIDType: "feishu_open_id",
			SenderID:     "ou_yinwm",
		}
		upper := lower
		upper.EntrypointID = "Feishu-Main"
		if PrincipalHash(lower) == PrincipalHash(upper) {
			t.Errorf("EntrypointID 大小写不同应产生不同 hash（逻辑 ID 区分大小写）")
		}
	})

	t.Run("sender 大小写不同 → hash 不同", func(t *testing.T) {
		lower := CronPrincipal{
			EntrypointID: "feishu-main",
			Channel:      "feishu",
			SenderIDType: "feishu_open_id",
			SenderID:     "ou_yinwm",
		}
		upper := CronPrincipal{
			EntrypointID: "feishu-main",
			Channel:      "feishu",
			SenderIDType: "feishu_open_id",
			SenderID:     "OU_YINWM",
		}
		if PrincipalHash(lower) == PrincipalHash(upper) {
			t.Errorf("SenderID 大小写不同应产生不同 hash（sender 不 lowercase）")
		}
	})

	t.Run("任一字段不同 → hash 不同", func(t *testing.T) {
		base := CronPrincipal{
			EntrypointID: "feishu-main",
			Channel:      "feishu",
			SenderIDType: "feishu_open_id",
			SenderID:     "ou_yinwm",
		}
		variants := []CronPrincipal{
			{EntrypointID: "feishu-other", Channel: "feishu", SenderIDType: "feishu_open_id", SenderID: "ou_yinwm"},
			{EntrypointID: "feishu-main", Channel: "ilink", SenderIDType: "feishu_open_id", SenderID: "ou_yinwm"},
			{EntrypointID: "feishu-main", Channel: "feishu", SenderIDType: "user_id", SenderID: "ou_yinwm"},
			{EntrypointID: "feishu-main", Channel: "feishu", SenderIDType: "feishu_open_id", SenderID: "ou_bob"},
		}
		baseHash := PrincipalHash(base)
		for i, v := range variants {
			if PrincipalHash(v) == baseHash {
				t.Errorf("variant %d 应产生不同 hash（字段不同）", i)
			}
		}
	})

	t.Run("hash 输出是 64 字符 hex（SHA-256）", func(t *testing.T) {
		p := CronPrincipal{
			EntrypointID: "feishu-main",
			Channel:      "feishu",
			SenderIDType: "feishu_open_id",
			SenderID:     "ou_yinwm",
		}
		h := PrincipalHash(p)
		if len(h) != 64 {
			t.Errorf("hash 长度 = %d, want 64 (SHA-256 hex)", len(h))
		}
		for _, c := range h {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Errorf("hash 含非 hex 字符 %q", c)
			}
		}
	})
}

package web

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPanfxStateFile 回归测试。
//
// 曾经的 bug：无论 env 文件在哪，都写成 filepath.Join(dir, "..", "data", ...)。
// 当 env 文件在项目根目录时（调试用的 -env _e2e.env），dir 是 "."，
// 再往上跳一级就写到了**项目的父目录**里，状态文件静默丢失。
func TestPanfxStateFile(t *testing.T) {
	orig := configFilePath
	t.Cleanup(func() { configFilePath = orig })

	t.Run("env 在 config/ 下", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "config"), 0o755); err != nil {
			t.Fatal(err)
		}
		configFilePath = filepath.Join(root, "config", "user.env")

		got := panfxStateFile()
		want := filepath.Join(root, "data", "panfx_replied.json")
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("env 在项目根目录", func(t *testing.T) {
		root := t.TempDir()
		configFilePath = filepath.Join(root, "user.env")

		got := panfxStateFile()
		// 关键：必须落在 root/data 下，绝不能是 root 的父目录
		if filepath.Dir(filepath.Dir(got)) != root {
			t.Fatalf("状态文件落在了项目目录之外: %q（root=%q）", got, root)
		}
		want := filepath.Join(root, "data", "panfx_replied.json")
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("相对路径 . 不越界", func(t *testing.T) {
		configFilePath = "user.env"
		got := panfxStateFile()
		if filepath.Dir(filepath.Dir(got)) != "." && filepath.Dir(got) != "data" {
			t.Fatalf("相对路径解析异常: %q", got)
		}
		if filepath.Dir(got) == ".." || filepath.Dir(got) == filepath.Join(".", "..") {
			t.Fatalf("不能写到上级目录: %q", got)
		}
	})
}

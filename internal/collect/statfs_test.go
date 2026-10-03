package collect

import (
	"strings"
	"testing"
)

// TestStatfsUsage_RealFilesystem 真的去问一次内核。
//
// 这条不是形式主义：statfsUsage 是整条链路里唯一直接调用系统调用的地方，
// 它的字段选择（Frsize 而不是 Bsize）无法用假实现验证。
func TestStatfsUsage_RealFilesystem(t *testing.T) {
	u, err := statfsUsage(".")
	if err != nil {
		t.Fatalf("statfsUsage(\".\") 失败: %v", err)
	}
	if u.Total == 0 {
		t.Error("Total 不应为 0")
	}
	if u.Used > u.Total {
		t.Errorf("Used(%d) 不应大于 Total(%d)", u.Used, u.Total)
	}
}

func TestStatfsUsage_NonexistentPath(t *testing.T) {
	_, err := statfsUsage("/这个路径肯定不存在/probe")
	if err == nil {
		t.Fatal("期望报错，却成功了")
	}
	if !strings.Contains(err.Error(), "no such file") {
		t.Logf("错误信息（接受任意错误）: %v", err)
	}
}

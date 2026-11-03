package collect

import (
	"strings"
	"testing"
)

// TestDiscoverMounts 覆盖自动发现的三件事：排除伪文件系统、按设备去重、排序。
func TestDiscoverMounts(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{
			name: "排除 proc/sysfs/tmpfs 这类伪文件系统",
			content: strings.Join([]string{
				"proc /proc proc rw 0 0",
				"sysfs /sys sysfs rw 0 0",
				"tmpfs /run tmpfs rw 0 0",
				"devtmpfs /dev devtmpfs rw 0 0",
				"cgroup2 /sys/fs/cgroup cgroup2 rw 0 0",
				"/dev/sda1 / ext4 rw 0 0",
			}, "\n"),
			want: []string{"/"},
		},
		{
			// Docker/K8s 里一次挂载会以好几种形式出现；同一块盘报三遍是纯噪音。
			name: "同一设备的多个 bind mount 只保留最浅的那个",
			content: strings.Join([]string{
				"/dev/sdb1 /root/.dsh ext4 rw 0 0",
				"/dev/sdb1 /data ext4 rw 0 0",
				"/dev/sdb1 /data/sub/deeper ext4 rw 0 0",
			}, "\n"),
			want: []string{"/data"},
		},
		{
			name: "多块盘按挂载点排序",
			content: strings.Join([]string{
				"/dev/sdc1 /mnt/backup xfs rw 0 0",
				"/dev/sda1 / ext4 rw 0 0",
				"/dev/sdb1 /data ext4 rw 0 0",
			}, "\n"),
			want: []string{"/", "/data", "/mnt/backup"},
		},
		{
			name:    "网络文件系统算存储，要保留",
			content: "nas.lan:/export /mnt/nas nfs4 rw 0 0\n//srv/share /mnt/smb cifs rw 0 0\n",
			want:    []string{"/mnt/nas", "/mnt/smb"},
		},
		{
			name:    "带空格的挂载点要还原八进制转义",
			content: "/dev/sda1 /mnt/my\\040disk ext4 rw 0 0",
			want:    []string{"/mnt/my disk"},
		},
		{
			name:    "没有真实存储时返回空（调用方负责退回根分区）",
			content: "proc /proc proc rw 0 0\ntmpfs /run tmpfs rw 0 0\n",
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DiscoverMounts(tt.content, nil)
			points := make([]string, 0, len(got))
			for _, m := range got {
				points = append(points, m.MountPoint)
			}
			if strings.Join(points, "|") != strings.Join(tt.want, "|") {
				t.Errorf("挂载点 = %v，期望 %v", points, tt.want)
			}
		})
	}
}

// TestDiscoverMounts_SkipsFileMounts 盯住容器里的一种真实形态：
// 宿主机把 /etc/hosts、/etc/resolv.conf 以**文件**形式 bind mount 进来。
// 它们背后的设备确实是磁盘，但把 "/etc/hosts" 当一块盘展示出来只会让人困惑。
func TestDiscoverMounts_SkipsFileMounts(t *testing.T) {
	content := strings.Join([]string{
		"/dev/vda1 /etc/hostname xfs rw 0 0",
		"/dev/vda1 /etc/hosts xfs rw 0 0",
		"/dev/vdb1 /workspace ext4 rw 0 0",
	}, "\n")

	files := map[string]bool{"/etc/hostname": false, "/etc/hosts": false, "/workspace": true}
	isDir := func(p string) bool { return files[p] }

	got := DiscoverMounts(content, isDir)
	if len(got) != 1 || got[0].MountPoint != "/workspace" {
		t.Fatalf("文件级 bind mount 应被跳过，只留目录挂载点，得到 %+v", got)
	}

	// isDir 传 nil 时不检查，两种挂载点都会留下来（纯解析场景）。
	if all := DiscoverMounts(content, nil); len(all) != 2 {
		t.Errorf("isDir=nil 时应保留设备去重后的 2 个，得到 %+v", all)
	}
}

func TestUnescapeMount(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/mnt/plain", "/mnt/plain"},
		{`/mnt/my\040disk`, "/mnt/my disk"},
		{`/mnt/tab\011here`, "/mnt/tab\there"},
		{`/mnt/back\134slash`, `/mnt/back\slash`},
		// 不完整的转义原样保留，不要吞字符。
		{`/mnt/bad\04`, `/mnt/bad\04`},
	}
	for _, tt := range tests {
		if got := unescapeMount(tt.in); got != tt.want {
			t.Errorf("unescapeMount(%q) = %q，期望 %q", tt.in, got, tt.want)
		}
	}
}

// TestPseudoFS_OverlayIsRealStorage 钉住一个容易搞反的判断。
//
// 容器里 / 就是 overlay。把 overlay 当伪文件系统排除掉的话，跑在 Docker 里的
// agent 会一块盘都发现不了。
func TestPseudoFS_OverlayIsRealStorage(t *testing.T) {
	if pseudoFS["overlay"] {
		t.Fatal("overlay 不能算伪文件系统——容器里的 / 就是它")
	}
	if !pseudoFS["tmpfs"] {
		t.Error("tmpfs 应被排除：那是内存，不是存储")
	}
	if !pseudoFS["proc"] {
		t.Error("proc 应被排除")
	}
}

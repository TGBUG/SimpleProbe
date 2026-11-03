package collect

import (
	"sort"
	"strings"
)

// Mount 是 /proc/mounts 一行里我们用得到的部分。
type Mount struct {
	Source     string
	MountPoint string
	FSType     string
}

// pseudoFS 是明确不算“存储”的文件系统类型。
//
// 用类型名而不是 f_type 魔数：/proc/mounts 已经给了可读类型名，而魔数还要
// 每个文件系统查一遍常量表。这份名单是**排除式**的——新出现的真实文件系统
// 默认会被纳入，只有已知的虚拟/内存文件系统才排除。
var pseudoFS = map[string]bool{
	// 内核伪文件系统：没有容量概念
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true,
	"cgroup": true, "cgroup2": true, "pstore": true, "securityfs": true,
	"debugfs": true, "tracefs": true, "configfs": true, "fusectl": true,
	"mqueue": true, "hugetlbfs": true, "bpf": true, "nsfs": true,
	"binfmt_misc": true, "autofs": true, "efivarfs": true, "selinuxfs": true,
	"rpc_pipefs": true, "nfsd": true, "procfs": true,
	// 内存文件系统：有容量但那是 RAM，不是存储
	"tmpfs": true, "ramfs": true, "devtmpfs_mqueue": true,
	// 只读镜像 / 发行版打包：snap、光驱，属于噪音
	"squashfs": true, "iso9660": true, "udf": true,
	// 容器内常见的 FUSE 杂项
	"fuse.lxcfs": true, "fuse.gvfsd-fuse": true,
}

// DiscoverMounts 从 /proc/mounts 的内容里挑出“真实存储”，返回挂载点列表。
//
// 四件事：
//   - 按类型排除虚拟/内存文件系统（见 pseudoFS）；
//   - **跳过不是目录的挂载点**：容器常把宿主机的 /etc/hosts、/etc/resolv.conf
//     以**文件**形式 bind mount 进来，它们背后的设备确实是磁盘，但把
//     "/etc/hosts" 当作一块盘展示出来只会让人困惑。isDir 传 nil 表示不检查。
//   - **按设备去重**：同一个文件系统被 bind mount 到多处时只保留一处。
//     Docker/K8s 里一次挂载会以好几种形式出现，不去重就会把同一块盘
//     重复报三四遍。
//   - 按挂载点排序，保证输出稳定（列表顺序就是面板顺序）。
//
// 代表挂载点取“路径层级最浅”的那个：绑定挂载通常是更深的路径，浅的那个
// 更可能是文件系统真正挂载的地方。
func DiscoverMounts(content string, isDir func(string) bool) []Mount {
	best := make(map[string]Mount)

	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		source := unescapeMount(fields[0])
		point := unescapeMount(fields[1])
		fstype := fields[2]

		if pseudoFS[fstype] || point == "" {
			continue
		}
		if isDir != nil && !isDir(point) {
			continue
		}

		key := source + "\x00" + fstype
		if prev, ok := best[key]; ok && !shallower(point, prev.MountPoint) {
			continue
		}
		best[key] = Mount{Source: source, MountPoint: point, FSType: fstype}
	}

	out := make([]Mount, 0, len(best))
	for _, m := range best {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MountPoint < out[j].MountPoint })
	return out
}

// shallower 判断 a 是否比 b 更适合作为代表挂载点：
// 先比路径层级，再比长度，最后用字典序保证结果确定。
func shallower(a, b string) bool {
	if da, db := strings.Count(a, "/"), strings.Count(b, "/"); da != db {
		return da < db
	}
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// unescapeMount 还原 /proc/mounts 里的八进制转义。
//
// 空格会被写成 \040，制表符 \011，换行 \012，反斜杠 \134。
// 不还原的话，路径里带空格的挂载点会抛 statfs 失败。
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) &&
			isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

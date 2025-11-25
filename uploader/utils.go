package uploader

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

func SafeGo(fn func()) {
	go func() {
		defer func() { recover() }()
		fn()
	}()
}

func FormatUnixPath(p string) string {
	v := filepath.VolumeName(p)
	vol := strings.TrimSuffix(v, ":")
	return "/" + vol + filepath.ToSlash(strings.TrimPrefix(p, v))
}

func FormatSize(b int64) string {
	const u = 1024
	if b < u {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(u), 0
	for n := b / u; n >= u; n /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func GetCreateTime(path string) int64 {
	h, err := windows.CreateFile(windows.StringToUTF16Ptr(path), windows.GENERIC_READ,
		windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return time.Now().Unix()
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(h, &info) != nil {
		return time.Now().Unix()
	}
	ft := uint64(info.CreationTime.HighDateTime)<<32 | uint64(info.CreationTime.LowDateTime)
	return int64(ft/10000000 - 11644473600)
}

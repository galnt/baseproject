package uploader

import (
	"net"
	"sync"
	"sync/atomic"
)

var (
	GlobalUploadSem = make(chan struct{}, 5)
	CheckSem        = make(chan struct{}, 20)

	ServerAddr string

	// 任务管理器是否开启
	TaskmgrRunning atomic.Bool
)

// 新增目录上传结构体
type UploadTask struct {
	ClientID  string
	RootPath  string
	FileQueue []string // 待上传文件列表

	Mu sync.Mutex

	ScanEnd bool // 新增：标记目录扫描是否已彻底结束（无论是自然结束还是因限额停止）

	connFunc func() (net.Conn, error) // 主线程的连接
}

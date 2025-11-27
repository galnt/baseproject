package uploader

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// 任务管理器是否开启
	TaskmgrRunning atomic.Bool

	ServerAddr string
	ClientName = "" // 定义全局用户名称
)

// 全局统一的上传队列和worker管理
var (
	GlobalFileQueue      chan FileQueueTask
	GlobalUploadWorkers  int32 = 0
	ActiveUploadTasks    int32 = 0 // 正在扫描中的任务数
	GlobalWorkerShutdown       = make(chan struct{})
	GlobalWorkerWG       sync.WaitGroup
	PendingFiles         atomic.Int64 // 新增：全局待上传文件计数
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

// 全局统一的文件任务结构体
type FileQueueTask struct {
	Path string
	Task *UploadTask // 保留引用，用于回调或统计（可选）
}

func init() {
	// 全局初始化一次
	GlobalFileQueue = make(chan FileQueueTask, 2000) // 全局缓冲2000
	startGlobalWorkers()                             // 程序启动时就启动20个worker

	// 在 main 或 init 中启动
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			pending := PendingFiles.Load()
			if pending > 0 || atomic.LoadInt32(&ActiveUploadTasks) > 0 {
				logMsg := fmt.Sprintf("\033[36m[全局进度] 待上传文件: %d | 活跃扫描任务: %d\033[0m", pending, atomic.LoadInt32(&ActiveUploadTasks))
				fmt.Println(logMsg)
			}
		}
	}()
}

// 调用接口的信息转换
func New(cfg Config, factory net.Conn) *Uploader {
	if cfg.MaxConcurrentUploads == 0 {
		cfg.MaxConcurrentUploads = 5
	}

	// 全局变量给值
	GlobalUploadSem = make(chan struct{}, cfg.MaxConcurrentUploads)
	ServerAddr = cfg.ServerAddr
	ClientName = cfg.ClientID

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

	// StartTaskManagerDetector(u.connFunc)

	// 检查路径是文件还是目录
	info, err := os.Stat(dirPath)
	if err != nil {
		// clog.LogMessage("路径错误：%v\n", err)
		return
	}

	// 流式目录上传
	task := &UploadTask{
		ClientID: ClientName,
		RootPath: dirPath,
		Conn:     u.connFunc,
	}

	if info.IsDir() {

		task.StartScanning()
	} else {
		task.doSendFileWithOutArray(dirPath)
	}
}

// 启动全局消费者
func startGlobalWorkers() {
	if atomic.CompareAndSwapInt32(&GlobalUploadWorkers, 0, 1) {
		for i := 0; i < MaxConcurrentUploads; i++ {
			GlobalWorkerWG.Add(1)
			go func(workerID int) {
				defer GlobalWorkerWG.Done()
				for {
					select {
					case ft, ok := <-GlobalFileQueue:
						if !ok {
							return
						}

						PendingFiles.Add(-1) // 取出就-1

						_ = ft.Task.doSendFile(ft.Path) // 直接复用原来的上传逻辑
					case <-GlobalWorkerShutdown:
						return
					}
				}
			}(i)
		}
	}
}

// 启动扫描 → 只负责把文件扔进全局队列
func (t *UploadTask) StartScanning() {
	atomic.AddInt32(&ActiveUploadTasks, 1)
	t.wg.Add(1)

	go func() {
		defer func() {
			t.ScanEnd.Store(true)
			atomic.AddInt32(&ActiveUploadTasks, -1)
			t.wg.Done()
		}()

		err := filepath.WalkDir(t.RootPath, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				logMsg := fmt.Sprintf("\033[31m访问路径失败 %q: %w\033[0m", path, err)
				fmt.Println(logMsg)
				return nil
			}

			PendingFiles.Add(1) // 投递前+1

			// 如果是目录,填充目录
			if d.IsDir() {
				path = "D|" + path
			}

			// 直接投递到全局队列（如果满了会阻塞，天然背压）
			GlobalFileQueue <- FileQueueTask{
				Path: path,
				Task: t,
			}

			return nil
		})

		if err != nil {
			logMsg := fmt.Sprintf("\033[31m目录扫描失败 %s: %v\033[0m", t.RootPath, err)
			fmt.Println(logMsg)
		} else {
			logMsg := fmt.Sprintf("\033[32m目录扫描完成: %s\033[0m", t.RootPath)
			fmt.Println(logMsg)
		}
	}()
}

// 等待当前任务彻底结束（扫描完 + 该任务的所有文件都上传完）
func (t *UploadTask) Wait() {
	t.wg.Wait()

	// 辅助等待：如果队列里还有本任务的文件，也要等
	for {
		if t.ScanEnd.Load() &&
			atomic.LoadInt32(&ActiveUploadTasks) == 0 &&
			PendingFiles.Load() == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// logMsg :=
	t.Conn.Write([]byte("NOTIFY\n" + fmt.Sprintf("\033[1;32m任务全部完成: %s\033[0m", t.RootPath) + "\n"))
}

// 当程序退出时调用（可选）
func ShutdownGlobalWorkers() {
	close(GlobalWorkerShutdown)
	GlobalWorkerWG.Wait()
	close(GlobalFileQueue)
}

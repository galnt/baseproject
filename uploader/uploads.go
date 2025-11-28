package uploader

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
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

var (
	// 新增：全局并发上传限制
	MaxConcurrentUploads = 5
	GlobalUploadSem      = make(chan struct{}, MaxConcurrentUploads) // 全局信号量，限制所有任务总并发

	MaxCheckUploadsSem = 20
	CheckSem           = make(chan struct{}, MaxCheckUploadsSem) // 新增：限制最多20个并发 FILECHECK

	SpeedLimit = 3 * 1024 * 1024 // 默认上传速度限制
	bufferSize = 32 * 1024       // 32KB缓冲区
)

// ==================== FILECHECK 专用连接池（关键优化） ====================
var (
	checkConnPool = sync.Pool{
		New: func() any { return nil }, // 初始占位
	}
	poolInitOnce sync.Once // 保证全局只初始化一次
)

// 必须在 uploader.New() 之后调用一次（可以调用 100 次都只生效一次）
func InitCheckConnPool() error {
	var err error
	poolInitOnce.Do(func() {
		if ServerAddr == "" {
			err = errors.New("ServerAddr 未设置，请先调用 uploader.New()")
			return
		}

		// 真正设置连接创建函数
		checkConnPool.New = func() any {
			conn, e := connectWithRetry(ServerAddr, nil)
			if e != nil {
				// 不要 panic，让调用方稍后自动重试
				return nil
			}

			// 在这里发送一个身份ID,这个SETUUID会在后端记录一个标识clientID,用于会话标识那个用户处理
			conn.Write([]byte("SETUUID\n" + ClientName + "\n"))
			return conn
		}

		// 可选：预热 8 条短连接，让第一次 FILECHECK 几乎无延迟
		for i := 0; i < MaxCheckUploadsSem; i++ {
			if c := checkConnPool.Get(); c != nil {
				if conn, ok := c.(net.Conn); ok && conn != nil {
					checkConnPool.Put(conn)
				}
			}
		}
	})
	return err
}

// 安全获取一个可用的连接（如果池子里暂时没有，会自动创建）
func getPooledConn() net.Conn {
	for {
		obj := checkConnPool.Get()
		if obj == nil {
			// 第一次使用或连接创建失败时触发 New()
			// 注意：checkConnPool.New() 在 InitCheckConnPool 中被设置
			// 这里调用 Get() 如果返回 nil，说明池中没有，且 New() 初始设置为返回 nil，所以需要再次调用 New 逻辑
			obj = checkConnPool.New()
		}
		if conn, ok := obj.(net.Conn); ok && conn != nil {
			return conn
		}
		// 创建失败，稍等后重试（网络抖动时非常有用）
		time.Sleep(100 * time.Millisecond)
	}
}

// 使用完毕后归还（不关闭，只放回池子）
func putPooledConn(c net.Conn) {
	if c == nil {
		return
	}
	c.SetDeadline(time.Time{}) // 清除 deadline，防止影响下一次使用
	checkConnPool.Put(c)
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

// 2025.09.24
// 修改 doSendFile 方法：签名改为 doSendFile(path string) error，在内部获取文件信息，并控制并发和 fConn 复用
func (t *UploadTask) doSendFile(fullPath string) error {
	// 获取 FILECHECK 信号量槽位（限制20个并发 FILECHECK）
	CheckSem <- struct{}{}
	defer func() { <-CheckSem }()

	// 使用连接池获取一条短连接
	fConn := getPooledConn()
	defer putPooledConn(fConn)
	// 设置短写入/读取超时以避免长时间阻塞（按需调整）
	fConn.SetDeadline(time.Now().Add(30 * time.Second)) // FILECHECK 阶段短超时

	// 判断是不是目录信息
	// ========== 1. 目录处理：以 "D|" 开头 ==========
	if strings.HasPrefix(fullPath, "D|") {
		fullPath := fullPath[2:] // 直接切掉 "D|"

		// 准备文件信息
		fileInfo, err := os.Stat(fullPath)
		if err != nil {
			return fmt.Errorf("获取文件信息失败: %w", err)
		}

		// 获取创建时间（跨平台）
		createTime := getCreateTime(fullPath)
		ct := strconv.FormatInt(createTime, 10)

		// 获取修改时间
		modTime := fileInfo.ModTime().Unix()
		mt := strconv.FormatInt(modTime, 10)

		cmd := fmt.Sprintf("MKDIR\n%s|%s|%s|%s\n", ClientName, formatUnixPath(fullPath), ct, mt)
		if _, err := fConn.Write([]byte(cmd)); err != nil {
			return fmt.Errorf("发送 MKDIR 失败: %w", err)
		}
		return nil
	}

	// 准备文件信息
	fileInfo, err := os.Stat(fullPath)
	if err != nil {
		return fmt.Errorf("获取文件信息失败: %w", err)
	}

	// 获取创建时间（跨平台）
	createTime := getCreateTime(fullPath)
	ct := strconv.FormatInt(createTime, 10)

	// 获取修改时间
	modTime := fileInfo.ModTime().Unix()
	mt := strconv.FormatInt(modTime, 10)

	// 发送CHECK命令并读取响应
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

	// <<<=== 新增：任务管理器打开时暂停上传 ===>>>
	for TaskmgrRunning.Load() {
		// t.Conn.Write([]byte("NOTIFY\n" + "任务管理器打开时暂停上传,待5秒后重试" + "\n"))
		NotifyServer("任务管理器打开时暂停上传,待5秒后重试")
		time.Sleep(5 * time.Second)
	}

	// FILECHECK 确认需要上传，获取上传信号量槽位（限制5个并发上传）
	GlobalUploadSem <- struct{}{}
	defer func() { <-GlobalUploadSem }()

	// 延长超时以适应文件上传,以链接24小时算
	fConn.SetDeadline(time.Now().Add(24 * time.Hour))

	// 发送协议头
	headers := []string{
		"FILE\n",
		fmt.Sprintf("%s|%d|%s|%s\n", formatUnixPath(fullPath), fileInfo.Size(), ct, mt),
	}
	for _, h := range headers {
		if _, err := fConn.Write([]byte(h)); err != nil {
			return fmt.Errorf("发送头部失败: %w", err)
		}
	}

	// 发送文件内容
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
		if _, err := t.rateLimitedCopyWithContext(fConn, src, limiter, fullPath); err != nil {
			return fmt.Errorf("传输失败: %w", err)
		}
	} else {
		if _, err := io.Copy(fConn, src); err != nil {
			return fmt.Errorf("传输失败: %w", err)
		}
	}

	// finalize logging
	counter.Finalize()
	return nil
}

// 强行上传,不需要进么排队处理,即单个上传逻辑
func (t *UploadTask) doSendFileWithOutArray(fullPath string) error {
	// 准备文件信息
	fileInfo, err := os.Stat(fullPath)
	if err != nil {
		return fmt.Errorf("获取文件信息失败: %w", err)
	}

	// 获取创建时间（跨平台）
	createTime := getCreateTime(fullPath)
	ct := strconv.FormatInt(createTime, 10)

	// 获取修改时间
	modTime := fileInfo.ModTime().Unix()
	mt := strconv.FormatInt(modTime, 10)

	/* 建立专用文件传输连接（带重试）
	fConn, err := connectWithRetry(ServerAddr, nil)
	if err != nil {
		// LogMessage(fmt.Sprintf("连接失败: %v", err))
		return fmt.Errorf("连接失败: %w", err)
	}
	defer fConn.Close()*/

	fConn := getPooledConn()
	defer putPooledConn(fConn)

	// 设置短写入/读取超时以避免长时间阻塞（按需调整）
	fConn.SetDeadline(time.Now().Add(30 * time.Minute))

	// 发送CHECK命令并读取响应
	if _, err := fConn.Write([]byte(fmt.Sprintf("FILECHECK\n%s|%s|%s|%s|%d\n", ClientName, formatUnixPath(fullPath), ct, mt, fileInfo.Size()))); err != nil {
		// LogMessage(fmt.Sprintf("发送CHECK失败: %v", err))
		return fmt.Errorf("发送CHECK失败: %w", err)
	}

	response, err := bufio.NewReader(fConn).ReadString('\n')
	if err != nil {
		// LogMessage(fmt.Sprintf("读取CHECK响应失败: %v", err))
		return fmt.Errorf("读取CHECK响应失败: %w", err)
	}

	if strings.TrimSpace(response) == "EXISTS" {
		// LogMessage(fmt.Sprintf("\033[33m跳过已存在文件: %s\033[0m", fullPath))
		return nil
	}

	// 发送协议头
	headers := []string{
		"FILE\n",
		fmt.Sprintf("%s|%d|%s|%s\n", formatUnixPath(fullPath), fileInfo.Size(), ct, mt),
	}
	for _, h := range headers {
		if _, err := fConn.Write([]byte(h)); err != nil {
			return fmt.Errorf("发送头部失败: %w", err)
		}
	}

	// 发送文件内容
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
		if _, err := t.rateLimitedCopyWithContext(fConn, src, limiter, fullPath); err != nil {
			return fmt.Errorf("传输失败: %w", err)
		}
	} else {
		if _, err := io.Copy(fConn, src); err != nil {
			return fmt.Errorf("传输失败: %w", err)
		}
	}

	// finalize logging
	counter.Finalize()
	return nil
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

// 替换原来的 rateLimitedCopyWithContext 为这个版本,实现强行断流
func (t *UploadTask) rateLimitedCopyWithContext(dst io.Writer, src io.Reader, limiter *rate.Limiter, fileName string) (written int64, err error) {
	buf := make([]byte, bufferSize) // 复用全局 bufferSize（32K~1M）
	ctx := context.Background()

	for {
		// 关键：每读一块就检查一次任务管理器！
		if TaskmgrRunning.Load() {
			// t.Conn.Write([]byte("NOTIFY\n" + "检测到任务管理器 → 暂停上传(FILECHECK正常)" + "\n"))
			NotifyServer(fmt.Sprintf("检测到任务管理器 → 暂停上传(FILECHECK正常),中断传输文件为: %s", filepath.Base(fileName)))
			return written, fmt.Errorf("任务管理器打开，主动中断传输")
		}

		nr, er := src.Read(buf)
		if nr > 0 {
			// 等待限速令牌
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
				return written, er
			}
			break
		}
	}
	return written, nil
}

func (c *WriteCounter) Write(p []byte) (int, error) {
	n := len(p)
	atomic.AddInt64(&c.Current, int64(n))

	// 节流控制：每200ms更新一次显示
	if time.Since(c.LastPrint) < 200*time.Millisecond {
		return n, nil
	}
	c.LastPrint = time.Now()

	/* 1 在传输过程中不进行日志打印
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
	}*/

	// 彩色终端输出
	// LogMessage("\r\033[33m%s\033[0m | 进度: \033[32m%.2f%%\033[0m | 速度: \033[36m%.2f KB/s\033[0m", c.FileName, progress, speed)
	return n, nil
}

// 最终完成显示
func (c *WriteCounter) Finalize() {
	current := atomic.LoadInt64(&c.Current)
	elapsed := time.Since(c.StartTime).Seconds()
	avgSpeed := float64(current) / elapsed / 1024

	logMsg := fmt.Sprintf("\r\033[1;32m%s\033[0m | 完成: \033[1;35m100.00%%\033[0m | 平均速度: \033[1;34m%.2f KB/s\033[0m", c.FileName, avgSpeed)
	fmt.Println(logMsg)
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
		fmt.Println(errMsg)

		select {
		case errChan <- errors.New(errMsg): // 非阻塞发送错误
		default:
		}

		// 等待重试（可加入context中断逻辑）
		time.Sleep(baseWaitTime) // 或使用带context的等待方式
	}
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

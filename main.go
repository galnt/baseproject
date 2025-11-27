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

	dirPath := "F:/m3u8"
	SpeedLimit := 10 * 1024 * 1024 // 默认上传速度限制

	ServerAddr := "1q502u2312.zicp.fun:8743"
	conn, _ := net.DialTimeout("tcp", ServerAddr, 10*time.Second)

	u := uploader.New(uploader.Config{
		ClientID:        ClientName,
		SpeedLimitBytes: int64(SpeedLimit),
		ServerAddr:      ServerAddr,
	}, conn)

	// 改成你想上传的目录或文件
	u.PatchDistribution(dirPath)

	select {}
}

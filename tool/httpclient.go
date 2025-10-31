package tool

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/proxy"
)

// const UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/84.0.4147.105 Safari/537.36"

const UserAgent = "Clash/v1.18.0"

type HttpClient struct {
	*http.Client
}

var httpClient *HttpClient

func init() {
	httpClient = &HttpClient{http.DefaultClient}
	httpClient.Timeout = time.Second * 90
}

// 在原方法的基础上添加代理访问网络请求
func GetHttpClient(proxyAddr string) *HttpClient {
	c := *httpClient

	// 添加 Socks5 代理配置
	// proxyAddr := "127.0.0.1:7890" // 替换为你的代理地址
	dialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, proxy.Direct)
	if err == nil {
		transport := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
		}
		c.Client.Transport = transport
	}

	// 设置 httpClient 的超时时间（例如 90 秒）
	c.Client.Timeout = time.Second * 90 // 你可以根据需要调整这个值

	return &c
}

func (c *HttpClient) Get(url string) (resp *http.Response, err error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("User-Agent", UserAgent)
	return c.Do(req)
}

func (c *HttpClient) Post(url string, body io.Reader) (resp *http.Response, err error) {
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("User-Agent", UserAgent)
	return c.Do(req)
}

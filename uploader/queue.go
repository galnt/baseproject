package uploader

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	MAX_QUEUE = 2000 // 队列上限，触发暂停
	RESUME_AT = 800  // 小于等于这个值时恢复扫描
)

// 生成文件列表并保存状态
func (t *UploadTask) GenerateFileList() error {
	t.Mu.Lock()
	t.FileQueue = t.FileQueue[:0] // 清空旧队列
	t.ScanEnd = false             // 重置扫描结束标志
	t.Mu.Unlock()

	var added int64 = 0
	var reachedLimit = false // 关键标志：是否曾经达到过 2000

	err := filepath.WalkDir(t.RootPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("访问路径失败 %q: %w", path, err)
		}
		if d.IsDir() {
			return nil
		}

		for {
			t.Mu.Lock()
			currentQueueLen := len(t.FileQueue)

			// 队列降到 <800，解除限制
			if currentQueueLen < RESUME_AT {
				reachedLimit = false
			}

			if reachedLimit {
				t.Mu.Unlock()
				OutputDebugString(fmt.Sprintf("队列已满，暂停扫描（%d），等待降至 < %d...", currentQueueLen, RESUME_AT))
				time.Sleep(1 * time.Second)
				continue
			}

			// 可以添加
			t.FileQueue = append(t.FileQueue, path)
			added++
			t.Mu.Unlock()

			// 添加后检查是否达到上限
			if len(t.FileQueue) >= MAX_QUEUE {
				reachedLimit = true
				OutputDebugString("队列达到 2000，触发限制，需降至 <800 才继续扫描")
			}

			break
		}

		return nil
	})

	// WalkDir 结束后，无论是因为正常结束还是错误，都标记扫描结束
	t.Mu.Lock()
	if err != nil {
		OutputDebugString(fmt.Sprintf("扫描出现错误: %v", err))
	}
	OutputDebugString(fmt.Sprintf("目录扫描完毕，已累计加入 %d 个文件", added))
	t.ScanEnd = true
	t.Mu.Unlock()

	return nil
}

func (t *UploadTask) startUploadWorkers() bool {
	var wg sync.WaitGroup

	for {
		t.Mu.Lock()
		if len(t.FileQueue) == 0 {
			// 队列为空时，判断扫描是否已经彻底结束
			if t.ScanEnd {
				t.Mu.Unlock()
				OutputDebugString("上传队列已空且目录扫描已结束，上传任务完成")
				break
			}
			// 否则说明扫描可能还在进行中，或者因限额暂停了
			t.Mu.Unlock()

			// 等待一小段时间再检查（避免空转 CPU）
			time.Sleep(3 * time.Second)
			continue
		}

		fullPath := t.FileQueue[0]
		t.FileQueue = t.FileQueue[1:]
		t.Mu.Unlock()

		wg.Add(1)

		/*
			go func(path string) {
				defer wg.Done()

				err := t.doSendFile(path)
				if err != nil {
					OutputDebugString(fmt.Sprintf("上传失败: %s, 错误: %v", path, err))
				}
			}(fullPath)*/

		// 正确写法（推荐）
		SafeGo(func() {
			p := fullPath // 必须复制一份
			if err := t.doSendFile(p); err != nil {
				OutputDebugString(fmt.Sprintf("上传失败: %s, 错误: %v", p, err))
			}
		})
	}

	wg.Wait()
	return true
}

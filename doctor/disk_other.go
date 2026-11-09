//go:build !windows && !linux && !darwin

package doctor

import "fmt"

func freeDisk(string) (uint64, error) {
	return 0, fmt.Errorf("当前平台不支持磁盘容量查询")
}

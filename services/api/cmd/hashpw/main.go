// Command hashpw 生成 bcrypt 密码哈希，用于给商户账号做种子数据或手工建号。
//
// 用法:
//
//	go run ./cmd/hashpw '你的密码'
//
// 密码不落日志、不进命令行历史之外的任何地方；输出的是哈希，不能反推原文。
package main

import (
	"fmt"
	"os"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "用法: hashpw <密码>")
		os.Exit(2)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(os.Args[1]), bcrypt.DefaultCost)
	if err != nil {
		fmt.Fprintf(os.Stderr, "生成哈希失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(hash))
}

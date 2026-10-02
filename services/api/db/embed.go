// Package db 只负责把迁移 SQL 文件嵌进二进制。
// 单独放一个包是因为 go:embed 不能引用父目录，而 migrations 又要与 queries 平级放在 db/ 下。
package db

import "embed"

//go:embed migrations/*.sql
var Migrations embed.FS

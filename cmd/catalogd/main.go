// catalogd：本地常驻元数据目录服务。
// 状态持久化到本地文件（每次变更原子落盘），进程重启后自动恢复，
// 通过 HTTP JSON API 对外提供摄入与查询，可用 curl 或脚本调用。
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"

	catalog "github.com/lisama569/metadata-catalog-go/catalog"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP 监听地址")
	data := flag.String("data", "catalog-data.json", "状态持久化文件路径")
	flag.Parse()

	c, err := catalog.Open(*data)
	if err != nil {
		log.Fatalf("打开持久化目录失败: %v", err)
	}
	fmt.Printf("catalogd 启动: addr=%s data=%s\n", *addr, *data)
	fmt.Println("端点: POST /api/entities /api/relations /api/evolve /api/merge")
	fmt.Println("      GET  /api/entities /api/entities/{id} /api/relations /api/rejections /api/impact?root=...")
	log.Fatal(http.ListenAndServe(*addr, c.Handler()))
}

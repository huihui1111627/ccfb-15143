// web 命令启动可视化服务。
//
//	go run ./cmd/web -addr :8080 -sample 20000
//
// 浏览器打开 http://localhost:8080 即可查看键分布、PLA 分段直线、
// 每段误差与一次点查的逐层预测过程。
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"

	"learnedindex/internal/web"
)

func main() {
	addr := flag.String("addr", ":8080", "监听地址")
	sample := flag.Int("sample", 20000, "可视化样本键数量（控制浏览器渲染压力）")
	flag.Parse()

	srv := web.New(*sample)
	fmt.Printf("可视化服务已启动：http://localhost%s （样本 %d 键）\n", *addr, *sample)
	log.Fatal(http.ListenAndServe(*addr, srv.Handler()))
}

// fake-cluster 는 로컬 Forgejo 3단계 각본(구현 계획 7.2)에 쓰는 가짜 ECS 와 가짜 Mimir 다. 운영에 쓰지 않는다.
//
//	:8080  ECS API(AWS_ENDPOINT_URL_ECS). 시험 클러스터와 관측 서비스 태스크
//	:9009  Mimir 조회(/prometheus/api/v1/query). 관측 서비스 태스크의 IP 가 이 컨테이너다
//	:8081  각본 제어. POST /scenario?name=<각본>, GET /state
//
// 시작할 때와 각본을 바꿀 때 moto(AWS_ENDPOINT_URL)에 버킷, SSM 두 값, SQS 큐를 만든다.
package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/Grandis-2/nova-loadtest/config"
	"github.com/Grandis-2/nova-loadtest/controller/internal/fakelocal"
)

func main() {
	if os.Getenv("AWS_ENDPOINT_URL") == "" {
		log.Fatal("AWS_ENDPOINT_URL(moto) 가 필요하다. 실제 AWS 에 닿지 않게 한다")
	}
	ctx := context.Background()
	cfg, err := config.Default()
	if err != nil {
		log.Fatal(err)
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatal(err)
	}
	s := &fakelocal.Server{
		S3:     s3.NewFromConfig(awsCfg, func(o *s3.Options) { o.UsePathStyle = true }),
		SSM:    ssm.NewFromConfig(awsCfg),
		SQS:    sqs.NewFromConfig(awsCfg),
		Config: cfg,
		SelfIP: selfIP(),
	}
	// moto 가 뜰 때까지 기다린다
	for i := 0; ; i++ {
		if err = s.Use(ctx, "pass"); err == nil {
			break
		}
		if i == 30 {
			log.Fatal(err)
		}
		log.Printf("moto 준비 대기: %v", err)
		time.Sleep(2 * time.Second)
	}
	ecsH, mimirH, controlH := s.Handlers()
	go func() { log.Fatal(http.ListenAndServe(":9009", mimirH)) }()
	go func() { log.Fatal(http.ListenAndServe(":8081", controlH)) }()
	log.Printf("fake-cluster: ECS :8080, Mimir %s:9009, 제어 :8081", s.SelfIP)
	log.Fatal(http.ListenAndServe(":8080", ecsH))
}

// selfIP 는 이 컨테이너의 IP 다. 컨트롤러가 관측 서비스 태스크 IP 로 받아 9009 로 조회한다
func selfIP() string {
	h, _ := os.Hostname()
	ips, err := net.LookupIP(h)
	if err == nil {
		for _, ip := range ips {
			if ip.To4() != nil && !ip.IsLoopback() {
				return ip.String()
			}
		}
	}
	return "127.0.0.1"
}

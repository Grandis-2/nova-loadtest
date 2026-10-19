// s3get은 e2e 시험에서 S3(moto) 객체를 서명한 요청으로 읽어 표준 출력에 쓴다.
//
//	s3get <버킷> <키>
package main

import (
	"context"
	"io"
	"log"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) != 3 {
		log.Fatal("사용법: s3get <버킷> <키>")
	}
	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatal(err)
	}
	c := s3.NewFromConfig(cfg, func(o *s3.Options) { o.UsePathStyle = true })
	out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(os.Args[1]), Key: aws.String(os.Args[2])})
	if err != nil {
		log.Fatal(err)
	}
	defer out.Body.Close()
	if _, err := io.Copy(os.Stdout, out.Body); err != nil {
		log.Fatal(err)
	}
}

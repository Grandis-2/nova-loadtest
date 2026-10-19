// Package awsio는 컨트롤러가 쓰는 S3, SQS 를 control 의 인터페이스에 맞춘다.
package awsio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/Grandis-2/nova-loadtest/controller/internal/control"
)

// Store는 시험 버킷이다.
type Store struct {
	Client *s3.Client
	Bucket string
}

func (s Store) Put(ctx context.Context, key string, body []byte) error {
	_, err := s.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.Bucket), Key: aws.String(key), Body: bytes.NewReader(body), ContentType: aws.String(contentType(key)),
	})
	return err
}

func contentType(key string) string {
	if len(key) > 3 && key[len(key)-3:] == ".md" {
		return "text/markdown; charset=utf-8"
	}
	return "application/json"
}

func (s Store) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.Client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(key)})
	if err != nil {
		var nk *s3types.NoSuchKey
		if errors.As(err, &nk) {
			return nil, control.ErrNotFound
		}
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

// DeletePrefix는 접두사 아래 객체를 모두 지운다(토큰 삭제).
func (s Store) DeletePrefix(ctx context.Context, prefix string) error {
	p := s3.NewListObjectsV2Paginator(s.Client, &s3.ListObjectsV2Input{Bucket: aws.String(s.Bucket), Prefix: aws.String(prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		if len(page.Contents) == 0 {
			continue
		}
		ids := make([]s3types.ObjectIdentifier, len(page.Contents))
		for i, o := range page.Contents {
			ids[i] = s3types.ObjectIdentifier{Key: o.Key}
		}
		out, err := s.Client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(s.Bucket), Delete: &s3types.Delete{Objects: ids}})
		if err != nil {
			return err
		}
		if len(out.Errors) > 0 {
			return fmt.Errorf("%s 지우기 실패: %s", aws.ToString(out.Errors[0].Key), aws.ToString(out.Errors[0].Message))
		}
	}
	return nil
}

// Queues는 SQS 큐의 남은 메시지를 센다. 이름으로 URL 을 찾는다(scenarios.yml 에는 이름만 있다).
type Queues struct{ Client *sqs.Client }

func (q Queues) Pending(ctx context.Context, names []string) (map[string]int64, error) {
	out := map[string]int64{}
	for _, name := range names {
		u, err := q.Client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		attrs, err := q.Client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl: u.QueueUrl,
			AttributeNames: []sqstypes.QueueAttributeName{
				sqstypes.QueueAttributeNameApproximateNumberOfMessages,
				sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
				sqstypes.QueueAttributeNameApproximateNumberOfMessagesDelayed,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		var n int64
		for _, v := range attrs.Attributes {
			c, _ := strconv.ParseInt(v, 10, 64)
			n += c
		}
		out[name] = n
	}
	return out, nil
}

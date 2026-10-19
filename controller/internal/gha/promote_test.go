package gha

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
)

// buildx 가 올리는 이미지 인덱스 형식
const indexType = "application/vnd.oci.image.index.v1+json"

type fakeECR struct {
	images map[string]string // 저장소:태그 → 매니페스트
	puts   []string
}

func (f *fakeECR) BatchGetImage(_ context.Context, in *ecr.BatchGetImageInput, _ ...func(*ecr.Options)) (*ecr.BatchGetImageOutput, error) {
	if len(in.AcceptedMediaTypes) > 0 {
		// ECR 은 단일 매니페스트 형식만 받는다. 인덱스를 넘기면 거절한다
		return nil, errors.New("InvalidParameterException: acceptedMediaTypes")
	}
	out := &ecr.BatchGetImageOutput{}
	key := aws.ToString(in.RepositoryName) + ":" + aws.ToString(in.ImageIds[0].ImageTag)
	if m, ok := f.images[key]; ok {
		out.Images = []ecrtypes.Image{{ImageManifest: aws.String(m), ImageManifestMediaType: aws.String(indexType)}}
	}
	return out, nil
}

func (f *fakeECR) PutImage(_ context.Context, in *ecr.PutImageInput, _ ...func(*ecr.Options)) (*ecr.PutImageOutput, error) {
	key := aws.ToString(in.RepositoryName) + ":" + aws.ToString(in.ImageTag)
	if f.images[key] == aws.ToString(in.ImageManifest) {
		return nil, &ecrtypes.ImageAlreadyExistsException{Message: aws.String("exists")}
	}
	if aws.ToString(in.ImageManifestMediaType) != indexType {
		return nil, errors.New("받은 매니페스트 형식을 그대로 넘겨야 한다")
	}
	f.images[key] = aws.ToString(in.ImageManifest)
	f.puts = append(f.puts, key)
	return &ecr.PutImageOutput{}, nil
}

func TestPromote(t *testing.T) {
	f := &fakeECR{images: map[string]string{}}
	for _, r := range Repos {
		f.images[r+":abc"] = "manifest-" + r
	}
	if err := Promote(context.Background(), f, Repos, "abc"); err != nil {
		t.Fatal(err)
	}
	for _, r := range Repos {
		if f.images[r+":latest"] != "manifest-"+r {
			t.Fatalf("%s:latest 가 커밋 태그와 같아야 한다", r)
		}
	}
	// 다시 돌려도 된다(latest 가 이미 같은 매니페스트)
	if err := Promote(context.Background(), f, Repos, "abc"); err != nil {
		t.Fatal(err)
	}
}

// 한 저장소라도 커밋 태그가 없으면 아무것도 옮기지 않는다(이미지가 섞이지 않게)
func TestPromoteMissingImage(t *testing.T) {
	f := &fakeECR{images: map[string]string{Repos[0] + ":abc": "m0", Repos[1] + ":abc": "m1"}}
	if err := Promote(context.Background(), f, Repos, "abc"); err == nil {
		t.Fatal("커밋 태그가 없는 저장소가 있으면 실패해야 한다")
	}
	if len(f.puts) != 0 {
		t.Fatalf("아무것도 옮기지 않아야 한다: %v", f.puts)
	}
}

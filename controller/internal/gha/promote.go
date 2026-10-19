package gha

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
)

// Repos 는 시험 이미지 세 저장소다(nova-infra 시험 스택 ecr.tf)
var Repos = []string{"nova/loadtest-token-minter", "nova/loadtest-k6", "nova/loadtest-controller"}

// ECR 은 promote 가 쓰는 ECR API 다
type ECR interface {
	BatchGetImage(context.Context, *ecr.BatchGetImageInput, ...func(*ecr.Options)) (*ecr.BatchGetImageOutput, error)
	PutImage(context.Context, *ecr.PutImageInput, ...func(*ecr.Options)) (*ecr.PutImageOutput, error)
}

// Promote 는 저장소마다 커밋 태그의 매니페스트를 latest 태그로 다시 쓴다.
// 레이어를 내려받지 않으므로 빌드 역할의 BatchGetImage, PutImage 권한만 쓴다
// (docker buildx imagetools create 는 레이어를 읽어 ecr:GetDownloadUrlForLayer 가 필요하다).
// 먼저 모든 저장소의 매니페스트를 읽고 나서 쓴다. 하나라도 없으면 아무것도 옮기지 않는다.
func Promote(ctx context.Context, c ECR, repos []string, sha string) error {
	type image struct{ repo, manifest, mediaType string }
	var images []image
	for _, repo := range repos {
		out, err := c.BatchGetImage(ctx, &ecr.BatchGetImageInput{
			// acceptedMediaTypes 는 넘기지 않는다. 넘길 수 있는 값은 단일 매니페스트 형식뿐이라 buildx 의 이미지 인덱스는
			// 고를 수 없고, 넘기지 않으면 ECR 은 저장된 매니페스트와 그 형식을 그대로 돌려준다(ECR 의 태그 다시 붙이기 안내와 같다)
			RepositoryName: aws.String(repo), ImageIds: []ecrtypes.ImageIdentifier{{ImageTag: aws.String(sha)}},
		})
		if err != nil {
			return fmt.Errorf("%s:%s 매니페스트: %w", repo, sha, err)
		}
		if len(out.Images) != 1 {
			return fmt.Errorf("%s:%s 이미지가 없다(%d개)", repo, sha, len(out.Images))
		}
		images = append(images, image{repo, aws.ToString(out.Images[0].ImageManifest), aws.ToString(out.Images[0].ImageManifestMediaType)})
	}
	for _, im := range images {
		in := &ecr.PutImageInput{RepositoryName: aws.String(im.repo), ImageTag: aws.String("latest"), ImageManifest: aws.String(im.manifest)}
		if im.mediaType != "" {
			in.ImageManifestMediaType = aws.String(im.mediaType) // 받은 형식 그대로 쓴다
		}
		_, err := c.PutImage(ctx, in)
		var exists *ecrtypes.ImageAlreadyExistsException
		if errors.As(err, &exists) {
			continue // latest 가 이미 이 커밋이다(다시 돌린 경우)
		}
		if err != nil {
			return fmt.Errorf("%s:latest: %w", im.repo, err)
		}
	}
	return nil
}

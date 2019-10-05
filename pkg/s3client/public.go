package s3client

import (
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/oxyno-zeta/s3-proxy/pkg/config"
	"github.com/sirupsen/logrus"
)

// NewS3Context New S3 Context
func NewS3Context(tgt *config.Target, logger *logrus.FieldLogger) (*S3Context, error) {
	sessionConfig := &aws.Config{
		Region: aws.String(tgt.Bucket.Region),
	}
	// Load credentials if they exists
	if tgt.Bucket.Credentials != nil && tgt.Bucket.Credentials.AccessKey != nil && tgt.Bucket.Credentials.SecretKey != nil {
		sessionConfig.Credentials = credentials.NewStaticCredentials(tgt.Bucket.Credentials.AccessKey.Value, tgt.Bucket.Credentials.SecretKey.Value, "")
	}
	// Load custom endpoint if it exists
	if tgt.Bucket.S3Endpoint != "" {
		sessionConfig.Endpoint = aws.String(tgt.Bucket.S3Endpoint)
		sessionConfig.S3ForcePathStyle = aws.Bool(true)
	}
	sess, err := session.NewSession(sessionConfig)
	if err != nil {
		return nil, err
	}
	svcClient := s3.New(sess)
	return &S3Context{svcClient: svcClient, logger: logger, Target: tgt}, nil
}

// ListFilesAndDirectories List files and directories
func (s3ctx *S3Context) ListFilesAndDirectories(key string) ([]*Entry, error) {
	// List files on path
	folders := make([]*Entry, 0)
	files := make([]*Entry, 0)
	err := s3ctx.svcClient.ListObjectsV2Pages(
		&s3.ListObjectsV2Input{
			Bucket:    aws.String(s3ctx.Target.Bucket.Name),
			Prefix:    aws.String(key),
			Delimiter: aws.String("/"),
		},
		func(page *s3.ListObjectsV2Output, lastPage bool) bool {
			// Manage folders
			for _, item := range page.CommonPrefixes {
				name := strings.TrimPrefix(*item.Prefix, key)
				folders = append(folders, &Entry{
					Type: FolderType,
					Key:  *item.Prefix,
					Name: name,
				})
			}
			// Manage files
			for _, item := range page.Contents {
				name := strings.TrimPrefix(*item.Key, key)
				if name != "" {
					files = append(files, &Entry{
						Type:         FileType,
						ETag:         *item.ETag,
						Name:         name,
						LastModified: *item.LastModified,
						Size:         *item.Size,
						Key:          *item.Key,
					})
				}
			}
			return lastPage
		})
	// Check if errors exists
	if err != nil {
		return nil, err
	}
	// Concat folders and files
	all := append(folders, files...)
	return all, nil
}

// GetObject Get object from S3 bucket
func (s3ctx *S3Context) GetObject(key string) (*ObjectOutput, error) {
	obj, err := s3ctx.svcClient.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(s3ctx.Target.Bucket.Name),
		Key:    aws.String(key),
	})
	if err != nil {
		// Try to cast error into an AWS Error if possible
		aerr, ok := err.(awserr.Error)
		if ok {
			switch aerr.Code() {
			case s3.ErrCodeNoSuchBucket, s3.ErrCodeNoSuchKey:
				return nil, ErrNotFound
			}
		}
		return nil, err
	}
	// Build output
	output := &ObjectOutput{
		Body: &obj.Body,
	}
	if obj.CacheControl != nil {
		output.CacheControl = *obj.CacheControl
	}
	if obj.Expires != nil {
		output.Expires = *obj.Expires
	}
	if obj.ContentDisposition != nil {
		output.ContentDisposition = *obj.ContentDisposition
	}
	if obj.ContentEncoding != nil {
		output.ContentEncoding = *obj.ContentEncoding
	}
	if obj.ContentLanguage != nil {
		output.ContentLanguage = *obj.ContentLanguage
	}
	if obj.ContentLength != nil {
		output.ContentLength = *obj.ContentLength
	}
	if obj.ContentRange != nil {
		output.ContentRange = *obj.ContentRange
	}
	if obj.ContentType != nil {
		output.ContentType = *obj.ContentType
	}
	if obj.ETag != nil {
		output.ETag = *obj.ETag
	}
	if obj.LastModified != nil {
		output.LastModified = *obj.LastModified
	}
	return output, nil
}
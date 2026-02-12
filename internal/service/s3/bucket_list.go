// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package s3

import (
	"context"
	"fmt"
	"iter"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	awstypes "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/hashicorp/terraform-plugin-framework/list"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-provider-aws/internal/errs/fwdiag"
	"github.com/hashicorp/terraform-provider-aws/internal/framework"
	"github.com/hashicorp/terraform-provider-aws/internal/logging"
	inttypes "github.com/hashicorp/terraform-provider-aws/internal/types"
	"github.com/hashicorp/terraform-provider-aws/names"
)

// Function annotations are used for list resource registration to the Provider. DO NOT EDIT.
// @SDKListResource("aws_s3_bucket")
func newBucketResourceAsListResource() inttypes.ListResourceForSDK {
	l := listResourceBucket{}
	l.SetResourceSchema(resourceBucket())
	return &l
}

var _ list.ListResource = &listResourceBucket{}

type listResourceBucket struct {
	framework.ListResourceWithSDKv2Resource
}

func (l *listResourceBucket) List(ctx context.Context, request list.ListRequest, stream *list.ListResultsStream) {
	conn := l.Meta().S3Client(ctx)

	var query listBucketModel
	if request.Config.Raw.IsKnown() && !request.Config.Raw.IsNull() {
		if diags := request.Config.Get(ctx, &query); diags.HasError() {
			stream.Results = list.ListResultsStreamDiagnostics(diags)
			return
		}
	}

	tflog.Info(ctx, "Listing S3 Bucket")
	stream.Results = func(yield func(list.ListResult) bool) {
		input := s3.ListBucketsInput{
			BucketRegion: aws.String(l.Meta().Region(ctx)),
			MaxBuckets:   aws.Int32(int32(request.Limit)),
		}
		
		// Collect all buckets first
		var allBuckets []awstypes.Bucket
		for item, err := range listBuckets(ctx, conn, &input) {
			if err != nil {
				result := fwdiag.NewListResultErrorDiagnostic(err)
				yield(result)
				return
			}
			allBuckets = append(allBuckets, item)
		}
		
		// Batch fetch tags using Resource Groups Tagging API
		tagsMap := l.fetchTagsInBatch(ctx, allBuckets)
		
		// Process buckets concurrently with a semaphore
		const concurrency = 10
		semaphore := make(chan struct{}, concurrency)
		
		type bucketResult struct {
			item awstypes.Bucket
			rd   *schema.ResourceData
			err  error
		}
		resultsChan := make(chan bucketResult, len(allBuckets))
		
		// Launch goroutines
		for _, bucket := range allBuckets {
			semaphore <- struct{}{} // Acquire
			go func(item awstypes.Bucket) {
				defer func() { <-semaphore }() // Release
				
				bucketName := aws.ToString(item.Name)
				
				rd := l.ResourceData()
				rd.SetId(bucketName)
				rd.Set(names.AttrBucket, bucketName)
				
				// Use the parent context, not the scoped one
				diags := resourceBucketRead(ctx, rd, l.Meta())
				
				var err error
				if diags.HasError() {
					err = fmt.Errorf("error reading bucket")
				}
				
				resultsChan <- bucketResult{item, rd, err}
			}(bucket)
		}
		
		// Collect all results
		results := make([]bucketResult, 0, len(allBuckets))
		
		for i := 0; i < len(allBuckets); i++ {
			results = append(results, <-resultsChan)
		}
		close(resultsChan)
		
		// Yield results
		for _, res := range results {
			bucketName := aws.ToString(res.item.Name)
			bucketCtx := tflog.SetField(ctx, logging.ResourceAttributeKey(names.AttrBucket), bucketName)

			result := request.NewListResult(bucketCtx)
			
			if res.err != nil || res.rd.Id() == "" {
				continue
			}
			
			// Set tags from batch fetch
			bucketARN := bucketARN(ctx, l.Meta(), bucketName)
			if tags, ok := tagsMap[bucketARN]; ok && len(tags) > 0 {
				res.rd.Set(names.AttrTags, tags)
				// Set tags_all to match tags (no provider-level default tags in list context)
				res.rd.Set("tags_all", tags)
			}

			result.DisplayName = bucketName

			l.SetResult(ctx, l.Meta(), request.IncludeResource, &result, res.rd)
			if result.Diagnostics.HasError() {
				yield(result)
				return
			}

			if !yield(result) {
				return
			}
		}
	}
}

// fetchTagsInBatch uses Resource Groups Tagging API to fetch tags for multiple S3 buckets at once
func (l *listResourceBucket) fetchTagsInBatch(ctx context.Context, buckets []awstypes.Bucket) map[string]map[string]string {
	tagsMap := make(map[string]map[string]string)
	
	if len(buckets) == 0 {
		return tagsMap
	}
	
	// Get Resource Groups Tagging API client
	conn := l.Meta().ResourceGroupsTaggingAPIClient(ctx)
	
	// Process in batches of 100 (API limit for ResourceARNList)
	const batchSize = 100
	for i := 0; i < len(buckets); i += batchSize {
		end := i + batchSize
		if end > len(buckets) {
			end = len(buckets)
		}
		
		batch := buckets[i:end]
		arns := make([]string, len(batch))
		for j, bucket := range batch {
			arns[j] = bucketARN(ctx, l.Meta(), aws.ToString(bucket.Name))
		}
		
		tflog.Debug(ctx, "Fetching S3 bucket tags in batch", map[string]interface{}{
			"batch_start": i,
			"batch_size":  len(arns),
		})
		
		// Use GetResources API to fetch tags for all ARNs at once
		input := &resourcegroupstaggingapi.GetResourcesInput{
			ResourceARNList: arns,
		}
		
		paginator := resourcegroupstaggingapi.NewGetResourcesPaginator(conn, input)
		pageNum := 0
		for paginator.HasMorePages() {
			pageNum++
			page, err := paginator.NextPage(ctx)
			if err != nil {
				tflog.Warn(ctx, "Failed to fetch S3 bucket tags batch", map[string]interface{}{
					"batch_start": i,
					"page":        pageNum,
					"error":       err.Error(),
				})
				break
			}
			
			tflog.Debug(ctx, "GetResources page received for S3", map[string]interface{}{
				"batch_start":    i,
				"page":           pageNum,
				"mappings_count": len(page.ResourceTagMappingList),
			})
			
			for _, mapping := range page.ResourceTagMappingList {
				arn := aws.ToString(mapping.ResourceARN)
				tags := make(map[string]string)
				for _, tag := range mapping.Tags {
					tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
				}
				if len(tags) > 0 {
					tagsMap[arn] = tags
				}
			}
		}
	}
	
	tflog.Info(ctx, "Batch tag fetch completed for S3", map[string]interface{}{
		"total_buckets": len(buckets),
		"tags_fetched":  len(tagsMap),
	})
	
	return tagsMap
}

type listBucketModel struct {
	framework.WithRegionModel
}

func listBuckets(ctx context.Context, conn *s3.Client, input *s3.ListBucketsInput) iter.Seq2[awstypes.Bucket, error] {
	return func(yield func(awstypes.Bucket, error) bool) {
		output, err := conn.ListBuckets(ctx, input)
		if err != nil {
			yield(awstypes.Bucket{}, fmt.Errorf("listing S3 Bucket resources: %w", err))
			return
		}

		for _, item := range output.Buckets {
			if !yield(item, nil) {
				return
			}
		}
	}
}

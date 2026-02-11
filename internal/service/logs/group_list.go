// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: MPL-2.0

package logs

import (
	"context"
	"fmt"
	"iter"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	awstypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/hashicorp/terraform-plugin-framework/list"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/hashicorp/terraform-provider-aws/internal/errs/fwdiag"
	"github.com/hashicorp/terraform-provider-aws/internal/framework"
	tfslices "github.com/hashicorp/terraform-provider-aws/internal/slices"
	inttypes "github.com/hashicorp/terraform-provider-aws/internal/types"
)

// @SDKListResource("aws_cloudwatch_log_group")
func newLogGroupResourceAsListResource() inttypes.ListResourceForSDK {
	l := logGroupListResource{}
	l.SetResourceSchema(resourceGroup())

	return &l
}

type logGroupListResource struct {
	framework.ListResourceWithSDKv2Resource
}

type logGroupListResourceModel struct {
	framework.WithRegionModel
}

func (l *logGroupListResource) List(ctx context.Context, request list.ListRequest, stream *list.ListResultsStream) {
	awsClient := l.Meta()
	conn := awsClient.LogsClient(ctx)

	var query logGroupListResourceModel
	if request.Config.Raw.IsKnown() && !request.Config.Raw.IsNull() {
		if diags := request.Config.Get(ctx, &query); diags.HasError() {
			stream.Results = list.ListResultsStreamDiagnostics(diags)
			return
		}
	}

	stream.Results = func(yield func(list.ListResult) bool) {
		startTime := time.Now()
		count := 0
		totalBatches := 0
		totalTagsTime := time.Duration(0)
		
		tflog.Info(ctx, "================================================")
		tflog.Info(ctx, "CLOUDWATCH LOGS LISTING - PERFORMANCE SUMMARY")
		tflog.Info(ctx, "================================================")
		
		result := request.NewListResult(ctx)
		var input cloudwatchlogs.DescribeLogGroupsInput
		
		// Collect log groups per page and fetch tags in batch
		currentBatch := []awstypes.LogGroup{}
		
		for output, err := range listLogGroups(ctx, conn, &input, tfslices.PredicateTrue[*awstypes.LogGroup]()) {
			if err != nil {
				result = fwdiag.NewListResultErrorDiagnostic(err)
				yield(result)
				return
			}
			
			currentBatch = append(currentBatch, output)
			
			// Process batch when we have 50 items (full page)
			if len(currentBatch) >= 50 {
				batchStart := time.Now()
				tagsMap := l.fetchTagsInBatch(ctx, currentBatch)
				batchElapsed := time.Since(batchStart)
				totalTagsTime += batchElapsed
				totalBatches++
				
				// Yield results for this batch
				for _, lg := range currentBatch {
					result := request.NewListResult(ctx)
					rd := l.ResourceData()
					rd.SetId(aws.ToString(lg.LogGroupName))
					resourceGroupFlatten(ctx, rd, lg)
					
					// Set tags from batch fetch
					arn := aws.ToString(lg.LogGroupArn)
					if tags, ok := tagsMap[arn]; ok && len(tags) > 0 {
						rd.Set("tags", tags)
					}
					
					result.DisplayName = aws.ToString(lg.LogGroupName)
					
					l.SetResult(ctx, awsClient, request.IncludeResource, &result, rd)
					if result.Diagnostics.HasError() {
						yield(result)
						return
					}
					
					count++
					
					if !yield(result) {
						return
					}
				}
				
				currentBatch = []awstypes.LogGroup{}
			}
		}
		
		// Process any remaining items in the last batch
		if len(currentBatch) > 0 {
			batchStart := time.Now()
			tagsMap := l.fetchTagsInBatch(ctx, currentBatch)
			batchElapsed := time.Since(batchStart)
			totalTagsTime += batchElapsed
			totalBatches++
			
			for _, lg := range currentBatch {
				result := request.NewListResult(ctx)
				rd := l.ResourceData()
				rd.SetId(aws.ToString(lg.LogGroupName))
				resourceGroupFlatten(ctx, rd, lg)
				
				arn := aws.ToString(lg.LogGroupArn)
				if tags, ok := tagsMap[arn]; ok && len(tags) > 0 {
					rd.Set("tags", tags)
				}
				
				result.DisplayName = aws.ToString(lg.LogGroupName)
				
				l.SetResult(ctx, awsClient, request.IncludeResource, &result, rd)
				if result.Diagnostics.HasError() {
					yield(result)
					return
				}
				
				count++
				
				if !yield(result) {
					return
				}
			}
		}
		
		elapsed := time.Since(startTime)
		tflog.Info(ctx, fmt.Sprintf("✓ Total batches processed: %d", totalBatches))
		tflog.Info(ctx, fmt.Sprintf("✓ Total tag fetch time: %s", totalTagsTime.Round(time.Millisecond)))
		tflog.Info(ctx, "================================================")
		tflog.Info(ctx, fmt.Sprintf("TOTAL TIME: %s", elapsed.Round(time.Millisecond)))
		tflog.Info(ctx, fmt.Sprintf("Log groups processed: %d", count))
		tflog.Info(ctx, fmt.Sprintf("Rate: %.1f log groups/second", float64(count)/elapsed.Seconds()))
		tflog.Info(ctx, "================================================")
	}
}

// fetchTagsInBatch uses Resource Groups Tagging API to fetch tags for multiple log groups at once
func (l *logGroupListResource) fetchTagsInBatch(ctx context.Context, logGroups []awstypes.LogGroup) map[string]map[string]string {
	tagsMap := make(map[string]map[string]string)
	
	if len(logGroups) == 0 {
		return tagsMap
	}
	
	// Get Resource Groups Tagging API client
	conn := l.Meta().ResourceGroupsTaggingAPIClient(ctx)
	
	// Process in batches of 100 (API limit for ResourceARNList)
	const batchSize = 100
	for i := 0; i < len(logGroups); i += batchSize {
		end := i + batchSize
		if end > len(logGroups) {
			end = len(logGroups)
		}
		
		batch := logGroups[i:end]
		arns := make([]string, len(batch))
		for j, lg := range batch {
			arns[j] = aws.ToString(lg.LogGroupArn)
		}
		
		tflog.Debug(ctx, "Fetching tags in batch", map[string]interface{}{
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
			// Apply rate limiting before each page fetch
			waitForGetResourcesRateLimit(ctx)
			
			pageNum++
			page, err := paginator.NextPage(ctx)
			if err != nil {
				tflog.Warn(ctx, "Failed to fetch tags batch", map[string]interface{}{
					"batch_start": i,
					"page":        pageNum,
					"error":       err.Error(),
				})
				break
			}
			
			tflog.Debug(ctx, "GetResources page received", map[string]interface{}{
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
	
	tflog.Info(ctx, "Batch tag fetch completed", map[string]interface{}{
		"total_resources": len(logGroups),
		"tags_fetched":    len(tagsMap),
	})
	
	return tagsMap
}

func listLogGroups(ctx context.Context, conn *cloudwatchlogs.Client, input *cloudwatchlogs.DescribeLogGroupsInput, filter tfslices.Predicate[*awstypes.LogGroup]) iter.Seq2[awstypes.LogGroup, error] {
	return func(yield func(awstypes.LogGroup, error) bool) {
		// Set explicit page size to maximum allowed by AWS API
		if input.Limit == nil {
			input.Limit = aws.Int32(50)
		}

		startTime := time.Now()
		pageCount := 0
		groupCount := 0

		tflog.Info(ctx, "Starting CloudWatch Log Groups listing", map[string]any{
			"page_size": aws.ToInt32(input.Limit),
			"rate_limit": "5 TPS (global across all CloudWatch Logs APIs)",
		})

		tflog.Debug(ctx, "Creating paginator")
		pages := cloudwatchlogs.NewDescribeLogGroupsPaginator(conn, input)
		
		tflog.Debug(ctx, "Entering pagination loop")
		for pages.HasMorePages() {
			tflog.Debug(ctx, "HasMorePages returned true, proceeding with request")
			
			// Rate limit before API call (DescribeLogGroups: 5 TPS)
			if err := waitForDescribeLogGroupsRateLimit(ctx); err != nil {
				yield(awstypes.LogGroup{}, err)
				return
			}

			tflog.Debug(ctx, "Calling DescribeLogGroups API", map[string]any{
				"page_number": pageCount + 1,
				"limit": aws.ToInt32(input.Limit),
			})

			page, err := pages.NextPage(ctx)
			if err != nil {
				tflog.Error(ctx, "Failed to list CloudWatch Log Groups", map[string]any{
					"error": err.Error(),
					"page_count": pageCount,
					"groups_retrieved": groupCount,
				})
				yield(awstypes.LogGroup{}, fmt.Errorf("listing CloudWatch Logs Log Groups: %w", err))
				return
			}

			pageCount++
			groupsInPage := len(page.LogGroups)
			groupCount += groupsInPage

			// Log progress every 10 pages (every ~500 log groups at 50/page)
			if pageCount%10 == 0 {
				elapsed := time.Since(startTime)
				tflog.Info(ctx, "CloudWatch Log Groups listing progress", map[string]any{
					"pages_retrieved": pageCount,
					"groups_retrieved": groupCount,
					"elapsed_seconds": int(elapsed.Seconds()),
					"groups_per_second": fmt.Sprintf("%.1f", float64(groupCount)/elapsed.Seconds()),
				})
			}

			for _, v := range page.LogGroups {
				if filter(&v) {
					if !yield(v, nil) {
						tflog.Info(ctx, "CloudWatch Log Groups listing stopped early", map[string]any{
							"pages_retrieved": pageCount,
							"groups_retrieved": groupCount,
						})
						return
					}
				}
			}
		}

		elapsed := time.Since(startTime)
		tflog.Info(ctx, "Completed CloudWatch Log Groups listing", map[string]any{
			"total_pages": pageCount,
			"total_groups": groupCount,
			"elapsed_seconds": int(elapsed.Seconds()),
			"average_groups_per_second": fmt.Sprintf("%.1f", float64(groupCount)/elapsed.Seconds()),
		})
	}
}

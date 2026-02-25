# `include_resource = false` Performance Fix — Implementation Plan

## Problem

Terraform Query list blocks support `include_resource = false` to fetch only resource
**identities** (ID, ARN, name) without full state. Many list implementations call a full
`resource*Read` or `find*` function **unconditionally** before `SetResult`, making expensive
AWS API calls even when the results are immediately discarded.

`SetResult(..., request.IncludeResource, ...)` only gates writing the resource body to output —
**any API calls made before `SetResult` still execute regardless of the flag.**

## How List Block Resources Were Found

List block resources carry a codegen annotation in their `*_list.go` file:
```bash
# Canonical command — finds all 48 list block resources
grep -rl "@SDKListResource\|@FrameworkListResource" internal/service --include="*.go" | sort
```

Validated against: https://hashicorp.github.io/terraform-provider-aws/add-a-new-list-resource/
- `skaff list --name <Name>` (SDK) or `skaff list --framework --name <Name>` (Framework) generates these
- Annotations are ONLY in `*_list.go` files — never in the parent resource file
- Parent resource carries `@SDKResource`/`@FrameworkResource` + `@IdentityAttribute(...)` annotations

## How Violations Were Found

### Step 1 — Broad grep for unguarded calls
```bash
for f in $(grep -rl "@SDKListResource\|@FrameworkListResource" internal/service --include="*.go"); do
  resource=$(grep -E '@(SDK|Framework)ListResource\("' "$f" | sed 's/.*"\(.*\)".*/\1/')
  grep -n "find[A-Z]\|resource[A-Z].*Read\|conn\.\|client\." "$f" \
    | grep -v "//\|Paginator\|NextPage\|HasMore\|IncludeResource\|Meta()\|conn :=\|Client("
  echo "=== $resource ==="
done
```

### Step 2 — Trace flatten functions for hidden API calls
`flatten*` and `resource*Flatten` functions may internally call `conn.*` / `find*`.
Grep the function body:
```bash
grep -n "conn\.\|client\.\|find[A-Z]" <function_body>
```
This found `resourceVPCFlatten` (6 calls) and `resourceInstanceFlatten` (4–8 calls)
which were initially missed as "clean."

### Step 3 — Verify identity fields from `@IdentityAttribute`
Check the parent resource file for `@IdentityAttribute` annotations — these define the
minimum fields that must always be set even when `include_resource = false`. Verify that
ALL such fields are available from the list API response without an extra call.

---

## Audit Results: 30 Resources Need Fixing

### ❌ Extra API calls per item — 30 resources

| Resource | Unguarded Call | Extra Calls | Identity Required |
|---|---|---|---|
| `aws_s3_bucket` | `resourceBucketRead` | **14** | `bucket` ← in list response ✅ |
| `aws_instance` | `resourceInstanceFlatten` | **4–8** | `id` ← in list response ✅ |
| `aws_vpc` | `resourceVPCFlatten` | **6** | `id` ← in list response ✅ |
| `aws_kms_key` | `findKeyInfo` | **3–4** | `id` ← in list response ✅ |
| `aws_lambda_function` | `findFunction` | **2+** | `function_name` ← in list response ✅ |
| `aws_route53_record` | `resourceRecordRead` | **2** | `zone_id`,`name`,`type`,`set_identifier` ← all in list ✅ |
| `aws_cleanrooms_collaboration` | `findCollaborationByID` | **2** | `id` ← in list response ✅ |
| `aws_secretsmanager_secret` | `resourceSecretRead` | **2** | `id` (ARN) ← in list response ✅ |
| `aws_ssm_parameter` | `resourceParameterRead` | **2** | `name`\* ← in list, needs `rd.Set` |
| `aws_appflow_flow` | `findFlowByName` | **1** | `name` ← in list response ✅ |
| `aws_cleanrooms_configured_table` | `findConfiguredTableByID` | **1** | `id` ← in list response ✅ |
| `aws_cloudfront_key_value_store` | `findKeyValueStoreByName` | **1** | `name`\* ← needs `data.Name=item.Name` |
| `aws_cloudwatch_event_rule` | `findRuleByTwoPartKey` | **1** | `name`\* ← needs `rd.Set("name",name)` |
| `aws_codebuild_project` | `resourceProjectRead` | **1** | `id` ← in list response ✅ |
| `aws_ecs_task_definition` | `findTaskDefinitionByFamilyOrARN` | **1** | `family`,`revision`\* ← parse from ARN |
| `aws_iam_role_policy` | `resourceRolePolicyRead` | **1** | `role`,`name`\* ← local vars, need `rd.Set` |
| `aws_lambda_permission` | `findPolicy` (per function) | **~0/item** | all inline ✅ (special case) |
| `aws_opensearchserverless_collection` | `findCollectionByID` | **1** | `id`\* ← needs `data.ID=summary.Id` |
| `aws_route` | `resourceRouteRead` | **1** | `route_table_id`,destination ← in list ✅ |
| `aws_route_table` | `resourceRouteTableRead` | **1** | `id` ← in list response ✅ |
| `aws_route53_resolver_rule_association` | `resourceRuleAssociationRead` | **1** | `id` ← in list response ✅ |
| `aws_s3_bucket_lifecycle_configuration` | `findBucketLifecycleConfiguration` | **1** | `bucket` ← in list response ✅ |
| `aws_s3_bucket_policy` | `findBucketPolicy` | **1** | `bucket` ← in list response ✅ |
| `aws_s3_bucket_public_access_block` | `resourceBucketPublicAccessBlockRead` | **1** | `bucket` ← in list response ✅ |
| `aws_s3_bucket_server_side_encryption_configuration` | `findServerSideEncryptionConfiguration` | **1** | `bucket` ← in list response ✅ |
| `aws_s3_directory_bucket` | `findDirectoryBucket` | **1** | `bucket`/`id` ← in list response ✅ |
| `aws_s3_object` | `resourceObjectRead` | **1** | `bucket`,`key` ← in list response ✅ |
| `aws_secretsmanager_secret_version` | `resourceSecretVersionRead` | **1+** | `secret_id`,`version_id` ← in list ✅ |
| `aws_security_group` | `resourceSecurityGroupRead` | **1** | `id` ← in list response ✅ |
| `aws_sqs_queue` | `resourceQueueRead` | **1** | `id` (URL) ← in list response ✅ |

\* = minor inline field assignment needed, data already in list response

### ✅ No fix needed — 18 resources
`aws_appflow_connector_profile`, `aws_batch_job_definition`, `aws_batch_job_queue`,
`aws_cloudwatch_event_target`, `aws_cloudwatch_log_group`, `aws_cloudwatch_metric_alarm`,
`aws_ec2_secondary_network`, `aws_ec2_secondary_subnet`, `aws_ecr_repository`,
`aws_iam_policy` *(guarded)*, `aws_iam_role`, `aws_iam_role_policy_attachment`,
`aws_kms_alias`, `aws_lambda_capacity_provider`, `aws_s3_bucket_acl` *(guarded)*,
`aws_subnet`, `aws_vpc_security_group_egress_rule`, `aws_vpc_security_group_ingress_rule`

---

## Identity Field Analysis

**All 30 violations can be fixed with zero additional API calls.**
Every `@IdentityAttribute` field is available from the list API response.

### Group A — 23 resources: Identity already set, just wrap the call
```go
// BEFORE (broken)
rd.SetId(bucketName)
rd.Set("bucket", bucketName)
diags := resourceBucketRead(ctx, rd, l.Meta())  // ← 14 calls, always runs

// AFTER (fixed)
rd.SetId(bucketName)
rd.Set("bucket", bucketName)
if request.IncludeResource {
    diags := resourceBucketRead(ctx, rd, l.Meta())
    // handle diags...
}
l.SetResult(ctx, l.Meta(), request.IncludeResource, &result, rd)
```

Resources: `aws_appflow_flow`, `aws_cleanrooms_collaboration`, `aws_cleanrooms_configured_table`,
`aws_codebuild_project`, `aws_instance`, `aws_kms_key`, `aws_lambda_function`,
`aws_route`, `aws_route_table`, `aws_route53_record`, `aws_route53_resolver_rule_association`,
`aws_s3_bucket`, `aws_s3_bucket_lifecycle_configuration`, `aws_s3_bucket_policy`,
`aws_s3_bucket_public_access_block`, `aws_s3_bucket_server_side_encryption_configuration`,
`aws_s3_directory_bucket`, `aws_s3_object`, `aws_secretsmanager_secret`,
`aws_secretsmanager_secret_version`, `aws_security_group`, `aws_sqs_queue`, `aws_vpc`

### Group B — 6 resources: Add one inline field, then wrap
Identity data is in the list response but not yet assigned. Add one line before the guard:

| Resource | Add before guard | Source |
|---|---|---|
| `aws_cloudwatch_event_rule` | `rd.Set("name", name)` | `name` var already in scope |
| `aws_cloudfront_key_value_store` | `data.Name = fwflex.StringToFramework(ctx, item.Name)` | `item.Name` |
| `aws_ecs_task_definition` | Parse `family`+`revision` from ARN | `arnStr` — format `.../family:revision` |
| `aws_iam_role_policy` | `rd.Set("role", roleName)` + `rd.Set("name", policyName)` | local vars in scope |
| `aws_opensearchserverless_collection` | `data.ID = fwflex.StringToFramework(ctx, collectionSummary.Id)` | `collectionSummary.Id` |
| `aws_ssm_parameter` | `rd.Set("name", name)` | `name` var already in scope (same as SetId value) |

### Group C — Special case
- `aws_lambda_permission`: `findPolicy` IS the enumeration mechanism (policies are parsed
  per function, not per statement). All identity fields set inline. ~0 extra calls per item.

---

## Reference Implementations

**Pattern A — Pure inline flatten (zero extra calls)**
`internal/service/logs/group_list.go` — fields flattened directly from `DescribeLogGroups`

**Pattern B — Guarded Read (acceptable)**
`internal/service/s3/bucket_acl_list.go` — identity set from list, Read guarded by `if request.IncludeResource`
`internal/service/iam/policy_list.go` — `findPolicyVersionByTwoPartKey` guarded by `if request.IncludeResource`

---

## Implementation Priority

| Priority | Resources | Why first |
|---|---|---|
| **P1** | `aws_s3_bucket` (14 calls), `aws_instance` (4–8), `aws_vpc` (6), `aws_kms_key` (3–4) | Highest call count per item |
| **P2** | `aws_route53_record`, `aws_cleanrooms_collaboration`, `aws_secretsmanager_secret`, `aws_ssm_parameter` | 2 calls or widely used |
| **P3** | All remaining 1-call Category A resources | Simple mechanical fix |
| **P4** | Group B resources (minor inline addition + guard) | Slightly more careful |
| **P5** | `aws_lambda_permission` | Amortized cost, harder to fix cleanly |

---

## Benchmark: Verifying the Fix

### How to count API calls (using HTTP interceptor)

Add a `countingTransport` to the provider's HTTP client before running a list query.
Reference the benchmark test template at:
`~/.copilot/session-state/1de60257-446d-42fe-892e-83cafff96ef6/files/benchmark_test.go`

### Expected results per resource after fix

| Resource | include_resource=false | include_resource=true |
|---|---|---|
| `aws_s3_bucket` | 1 call (ListBuckets page) | 1 + 14 per bucket |
| `aws_instance` | 1 call (DescribeInstances page) | 1 + 4–8 per instance |
| `aws_vpc` | 1 call (DescribeVpcs page) | 1 + 6 per VPC |
| `aws_kms_key` | 1 call (ListKeys page) | 1 + 3–4 per key |
| `aws_lambda_function` | 1 call (ListFunctions page) | 1 + 2+ per function |
| All single-call resources | 1 call (list page) | 1 + 1 per item |

### Running the benchmark
```bash
# Run benchmark for a specific service (requires AWS credentials)
make testacc PKG=s3 TESTARGS='-run=TestAccS3Bucket_List_IncludeResourcePerf'

# Or run the standalone benchmark (no AWS needed — uses HTTP interceptor + mock)
go test -v -run=TestBenchmarkListResource ./internal/service/...
```

### Adding `TestAcc_List_includeResource` tests
The `include_resource = false` behavior is tested via `list_basic` testdata — the
`tfquerycheck.ExpectNoResourceObject` assertion confirms no resource body is included.
After fixing each resource, also add `list_include_resource/query.tfquery.hcl`:
```hcl
list "aws_xxx" "test" {
  provider = aws
  include_resource = true
}
```

---

## Full List of 48 Supported Resources (as of 2026-02-25)

| Resource | Package | Query Fields |
|---|---|---|
| `aws_appflow_connector_profile` | `appflow` | `region` |
| `aws_appflow_flow` | `appflow` | `region` |
| `aws_batch_job_definition` | `batch` | `region` |
| `aws_batch_job_queue` | `batch` | `region` |
| `aws_cleanrooms_collaboration` | `cleanrooms` | `region` |
| `aws_cleanrooms_configured_table` | `cleanrooms` | `region` |
| `aws_cloudfront_key_value_store` | `cloudfront` | *(none — global service)* |
| `aws_cloudwatch_event_rule` | `events` | `region` |
| `aws_cloudwatch_event_target` | `events` | `region`, `event_bus_name`, `rule` |
| `aws_cloudwatch_log_group` | `logs` | `region` |
| `aws_cloudwatch_metric_alarm` | `cloudwatch` | `region` |
| `aws_codebuild_project` | `codebuild` | `region` |
| `aws_ec2_secondary_network` | `ec2` | `region` |
| `aws_ec2_secondary_subnet` | `ec2` | `region`, `filter` |
| `aws_ecr_repository` | `ecr` | `region` |
| `aws_ecs_task_definition` | `ecs` | `region` |
| `aws_iam_policy` | `iam` | `path_prefix` |
| `aws_iam_role` | `iam` | *(none — global service)* |
| `aws_iam_role_policy` | `iam` | `role_name` |
| `aws_iam_role_policy_attachment` | `iam` | *(none — global service)* |
| `aws_instance` | `ec2` | `region`, `filter`, `include_auto_scaled` |
| `aws_kms_alias` | `kms` | `region` |
| `aws_kms_key` | `kms` | `region` |
| `aws_lambda_capacity_provider` | `lambda` | `region` |
| `aws_lambda_function` | `lambda` | `region` |
| `aws_lambda_permission` | `lambda` | `region`, `function_name`, `qualifier` |
| `aws_opensearchserverless_collection` | `opensearchserverless` | `region` |
| `aws_route` | `ec2` | `region`, `route_table_id` |
| `aws_route_table` | `ec2` | `region`, `route_table_ids`, `filter` |
| `aws_route53_record` | `route53` | `zone_id` |
| `aws_route53_resolver_rule_association` | `route53resolver` | `region` |
| `aws_s3_bucket` | `s3` | `region` |
| `aws_s3_bucket_acl` | `s3` | `region` |
| `aws_s3_bucket_lifecycle_configuration` | `s3` | `region` |
| `aws_s3_bucket_policy` | `s3` | `region` |
| `aws_s3_bucket_public_access_block` | `s3` | `region` |
| `aws_s3_bucket_server_side_encryption_configuration` | `s3` | `region` |
| `aws_s3_directory_bucket` | `s3` | `region` |
| `aws_s3_object` | `s3` | `region`, `bucket`, `prefix` |
| `aws_secretsmanager_secret` | `secretsmanager` | `region` |
| `aws_secretsmanager_secret_version` | `secretsmanager` | `region`, `secret_id` |
| `aws_security_group` | `ec2` | `region`, `group_ids`, `filter` |
| `aws_sqs_queue` | `sqs` | `region` |
| `aws_ssm_parameter` | `ssm` | `region` |
| `aws_subnet` | `ec2` | `region`, `subnet_ids`, `filter` |
| `aws_vpc` | `ec2` | `region`, `vpc_ids`, `filter` |
| `aws_vpc_security_group_egress_rule` | `ec2` | `region`, `security_group_rule_ids`, `filter` |
| `aws_vpc_security_group_ingress_rule` | `ec2` | `region`, `security_group_rule_ids`, `filter` |

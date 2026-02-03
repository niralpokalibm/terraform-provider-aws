# Copyright IBM Corp. 2014, 2026
# SPDX-License-Identifier: MPL-2.0

resource "aws_{{ .ServicePackage }}_{{ .ListResourceSnake }}" "test" {
{{- if .IsRegionOverride }}
  count  = var.resource_count
  region = var.region
{{ else }}
  count = var.resource_count
{{ end }}
  name = "${var.rName}-${count.index}"
}

variable "rName" {
  description = "Name for resource"
  type        = string
  nullable    = false
}

variable "resource_count" {
  description = "Number of resources to create"
  type        = number
  nullable    = false
}
{{ if .IsRegionOverride }}
variable "region" {
  description = "Region to deploy resource in"
  type        = string
  nullable    = false
}
{{ end -}}

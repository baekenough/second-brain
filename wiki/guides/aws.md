---
title: "Guide: AWS"
type: guide
updated: 2026-04-12
sources:
  - guides/aws/common-patterns.md
related:
  - "[[infra-aws-expert]]"
  - "[[infra-docker-expert]]"
  - "[[skills/aws-best-practices]]"
---

# Guide: AWS

Reference documentation for AWS cloud architecture patterns, Well-Architected Framework, and infrastructure-as-code templates.

## Overview

The AWS guide provides reference documentation for `infra-aws-expert` and the `aws-best-practices` skill. It covers common architecture patterns (three-tier web, serverless, data pipeline) and Well-Architected Framework implementations across the five pillars.

## Key Topics

- **Architecture Patterns**: Three-tier web application, serverless, microservices, data pipeline
- **Well-Architected Framework**: Operational excellence, security, reliability, performance, cost optimization
- **IaC Templates**: CloudFormation, CDK (Python/TypeScript), Terraform module patterns
- **Networking**: VPC design, subnet segmentation, security groups, NACLs
- **Compute**: EC2 sizing, ECS Fargate task definitions, Lambda function patterns
- **Security**: IAM policy least-privilege, KMS encryption, Secrets Manager integration
- **Cost**: Reserved Instances, Savings Plans, right-sizing recommendations

## Relationships

- **Agent**: [[infra-aws-expert]] primary consumer
- **Skill**: [[skills/aws-best-practices]] implements patterns from this guide
- **Containerization**: [[infra-docker-expert]] for ECS/EKS container packaging

## Sources

- `guides/aws/common-patterns.md`

---
title: infra-aws-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/infra-aws-expert.md
related:
  - "[[infra-docker-expert]]"
  - "[[mgr-gitnerd]]"
  - "[[skills/aws-best-practices]]"
  - "[[r001]]"
---

# infra-aws-expert

AWS cloud architect following the Well-Architected Framework for IaC (CloudFormation/CDK/Terraform), VPC networking, IAM security, and cost optimization.

## Overview

`infra-aws-expert` designs and implements AWS cloud infrastructure with the Well-Architected Framework's five pillars as the guiding standard: operational excellence, security, reliability, performance efficiency, and cost optimization. It handles the full stack from VPC networking through compute (EC2, ECS, Lambda) and security (IAM, KMS).

Memory is user-scoped because AWS infrastructure patterns (VPC design, IAM policies) apply across projects.

## Key Details

- **Model**: sonnet | **Effort**: high | **Memory**: user
- **Domain**: devops | **Skill**: aws-best-practices | **Guide**: `guides/aws/`

### Capabilities

1. Well-Architected Framework architecture design
2. Infrastructure as Code: CloudFormation, CDK (Python/TypeScript), Terraform
3. Networking: VPC, subnets, security groups, NACLs, Transit Gateway
4. Compute: EC2, ECS (Fargate), Lambda, Auto Scaling
5. Security: IAM policies, KMS encryption, Secrets Manager
6. Cost optimization: Reserved Instances, Savings Plans, right-sizing

## Relationships

- **Containerization**: [[infra-docker-expert]] for container images deployed to AWS (ECS/EKS)
- **CI/CD integration**: [[mgr-gitnerd]] for GitHub Actions → AWS deployment pipelines
- **Security rules**: [[r001]] for prohibited actions (no AWS credential exposure)

## Sources

- `.claude/agents/infra-aws-expert.md`

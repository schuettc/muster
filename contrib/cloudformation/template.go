// Package cloudformation exposes muster's hosted-backend CloudFormation
// template to Go code that has to ship it — today that is cmd/muster-deploy,
// which embeds the stack rather than asking the operator to have a checkout.
//
// The template lives here rather than under internal/ so that the path every
// document already gives operators — contrib/cloudformation/muster-backend.yaml
// — stays the real file. `aws cloudformation deploy --template-file` against
// this path and `muster-deploy` therefore deploy the SAME bytes; there is no
// generated copy to drift from the original, which is the whole reason this
// one-line package exists instead of a duplicate under the deploy tool.
//
// This package imports nothing and must keep it that way: it is reachable
// from cmd/muster-deploy only, and nothing here may pull the AWS SDK into a
// package the device binary can see.
package cloudformation

import _ "embed"

// Template is the hosted-backend stack: DynamoDB table, Lambda function, the
// HTTP API in front of it, and a least-privilege execution role.
//
//go:embed muster-backend.yaml
var Template string

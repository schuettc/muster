//go:build lambda

package main

import "github.com/schuettc/muster/internal/lambdamode"

// runLambda serves the AWS Lambda runtime. Built only under the `lambda` tag:
// it is the sole path that pulls the AWS SDK in, and the default binary that
// every device runs must not carry it.
func runLambda() int { return lambdamode.Run() }

//go:build !lambda

package main

import (
	"fmt"
	"os"
)

// runLambda reports that this binary was built without lambda mode. The AWS
// SDK ships only in the Lambda release artifact (built with `-tags lambda`),
// so the default binary omits it entirely — see the plan's Global Constraints.
func runLambda() int {
	fmt.Fprintln(os.Stderr, "muster: this binary was built without lambda mode "+
		"(rebuild with -tags lambda; the released muster-lambda-*.zip already has it)")
	return 2
}

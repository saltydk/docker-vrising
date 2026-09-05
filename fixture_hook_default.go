//go:build !fixture

package main

import "context"

func fixtureAfterManagedPublication(context.Context, string) error {
	return nil
}

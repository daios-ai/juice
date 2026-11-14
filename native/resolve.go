// SPDX-License-Identifier: AGPL-3.0-only

package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// Resolve declares @sys/resolve (D17): a program's name service. A user's address, written with a
// handle or as a principal, in; that user's address as this kernel names them and their principal,
// the same on every kernel, out — what a program keeps and sends instead of a name (D15).
func Resolve() Spec {
	return Spec{
		Name:         "resolve",
		Title:        "Resolve a user",
		Description:  "Turns a user's address into their principal, which names them on every kernel, and the address this kernel shows them by",
		InputSchema:  obj(map[string]any{"address": str("The user, as handle@kernel or user_id@kernel_key")}, "address"),
		OutputSchema: addressSchema("The user, as this kernel names them and as their principal"),
		Handler: func(k Host) kernel.NativeFunc {
			return func(ctx context.Context, args map[string]any, _, _, _, _, _ string, _ kernel.Parties) (map[string]any, error) {
				address, _ := args["address"].(string)
				p, err := k.ResolvePrincipal(ctx, address)
				if err != nil {
					return nil, err
				}
				return asJSONObject(k.NewNames().Address(ctx, p)), nil
			}
		},
	}
}

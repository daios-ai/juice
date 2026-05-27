package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

func init() {
	userCmd := &cobra.Command{Use: "user", Short: "User account commands"}
	userCmd.AddCommand(userCreateCmd())
	userCmd.AddCommand(userMeCmd())
	rootCmd.AddCommand(userCmd)
}

func userCreateCmd() *cobra.Command {
	var handle, email, password string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new user account",
		RunE: func(_ *cobra.Command, _ []string) error {
			if password == "" {
				p, err := promptPassword("Password: ")
				if err != nil {
					return err
				}
				password = p
			}

			k, db, err := openKernel()
			if err != nil {
				return err
			}
			defer db.Close()

			u, err := k.CreateUser(context.Background(), kernel.CreateUserRequest{
				Handle:   handle,
				Email:    email,
				Password: password,
			})
			if err != nil {
				return err
			}

			if flagOutput == "json" {
				return printJSON(u)
			}
			fmt.Printf("User created: %s (id: %s)\n", u.Handle, u.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&handle, "handle", "", "Unique handle, e.g. @alice (required)")
	cmd.Flags().StringVar(&email, "email", "", "Email address (required)")
	cmd.Flags().StringVar(&password, "password", "", "Password (prompted if omitted)")
	_ = cmd.MarkFlagRequired("handle")
	_ = cmd.MarkFlagRequired("email")
	return cmd
}

func userMeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "me",
		Short: "Show the authenticated user's profile",
		RunE: func(_ *cobra.Command, _ []string) error {
			k, db, err := openKernel()
			if err != nil {
				return err
			}
			defer db.Close()

			subjectID, err := requireSubjectID(k)
			if err != nil {
				return err
			}

			u, err := k.ReadUser(context.Background(), subjectID)
			if err != nil {
				return err
			}

			if flagOutput == "json" {
				return printJSON(map[string]any{
					"id":        u.ID,
					"handle":    u.Handle,
					"email":     u.Email,
					"available": u.Available,
					"locked":    u.Locked,
				})
			}
			fmt.Printf("id:        %s\nhandle:    %s\nemail:     %s\navailable: %d\nlocked:    %d\n",
				u.ID, u.Handle, u.Email, u.Available, u.Locked)
			return nil
		},
	}
}

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

func init() {
	actionCmd := &cobra.Command{Use: "action", Short: "Action management commands"}
	actionCmd.AddCommand(
		actionAddCmd(),
		actionUpdateCmd(),
		actionEnableCmd(),
		actionDisableCmd(),
		actionListCmd(),
		actionDeleteCmd(),
		actionACLCmd(),
		actionGrantAllCmd(),
		actionRevokeAllCmd(),
	)
	rootCmd.AddCommand(actionCmd)
}

func actionAddCmd() *cobra.Command {
	var name, kind, source, description string
	var price int64
	var inputSchemaStr, outputSchemaStr string
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Create a new action",
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

			inputSchema := map[string]any{}
			if inputSchemaStr != "" {
				if err := json.Unmarshal([]byte(inputSchemaStr), &inputSchema); err != nil {
					return fmt.Errorf("invalid --input-schema: %w", err)
				}
			}
			outputSchema := map[string]any{}
			if outputSchemaStr != "" {
				if err := json.Unmarshal([]byte(outputSchemaStr), &outputSchema); err != nil {
					return fmt.Errorf("invalid --output-schema: %w", err)
				}
			}

			srcData := source
			if source != "" {
				if _, err := os.Stat(source); err == nil {
					data, err := os.ReadFile(source)
					if err != nil {
						return fmt.Errorf("reading source file: %w", err)
					}
					srcData = string(data)
				}
			}

			a, err := k.CreateAction(context.Background(), kernel.CreateActionRequest{
				OwnerUserID:  subjectID,
				Name:         name,
				Kind:         kernel.ActionKind(kind),
				Price:        price,
				Description:  description,
				InputSchema:  inputSchema,
				OutputSchema: outputSchema,
				Source:       srcData,
			})
			if err != nil {
				return err
			}

			if flagOutput == "json" {
				return printJSON(a)
			}
			fmt.Printf("Action created: %s (id: %s)\n", a.Name, a.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Action name, e.g. /hello (required)")
	cmd.Flags().StringVar(&kind, "kind", "http", "Action kind: http, wasm, native")
	cmd.Flags().StringVar(&source, "source", "", "URL (http) or file path (wasm)")
	cmd.Flags().StringVar(&description, "description", "", "Human-readable description")
	cmd.Flags().Int64Var(&price, "price", 0, "Price in credits")
	cmd.Flags().StringVar(&inputSchemaStr, "input-schema", "", "JSON Schema for inputs")
	cmd.Flags().StringVar(&outputSchemaStr, "output-schema", "", "JSON Schema for outputs")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func actionUpdateCmd() *cobra.Command {
	var actionID, description, source string
	var price int64
	var inputSchemaStr, outputSchemaStr string
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update an action's metadata",
		RunE: func(c *cobra.Command, _ []string) error {
			k, db, err := openKernel()
			if err != nil {
				return err
			}
			defer db.Close()

			subjectID, err := requireSubjectID(k)
			if err != nil {
				return err
			}

			req := kernel.UpdateActionRequest{ID: actionID}

			if c.Flags().Changed("description") {
				req.Description = &description
			}
			if c.Flags().Changed("source") {
				req.Source = &source
			}
			if c.Flags().Changed("price") {
				req.Price = &price
			}
			if inputSchemaStr != "" {
				m := map[string]any{}
				if err := json.Unmarshal([]byte(inputSchemaStr), &m); err != nil {
					return fmt.Errorf("invalid --input-schema: %w", err)
				}
				req.InputSchema = m
			}
			if outputSchemaStr != "" {
				m := map[string]any{}
				if err := json.Unmarshal([]byte(outputSchemaStr), &m); err != nil {
					return fmt.Errorf("invalid --output-schema: %w", err)
				}
				req.OutputSchema = m
			}

			a, err := k.UpdateAction(context.Background(), subjectID, req)
			if err != nil {
				return err
			}

			if flagOutput == "json" {
				return printJSON(a)
			}
			fmt.Printf("Action %s updated (active=%v).\n", a.Name, a.Active)
			return nil
		},
	}
	cmd.Flags().StringVar(&actionID, "id", "", "Action ID (required)")
	cmd.Flags().StringVar(&description, "description", "", "New description")
	cmd.Flags().StringVar(&source, "source", "", "New source URL or file path")
	cmd.Flags().Int64Var(&price, "price", 0, "New price in credits")
	cmd.Flags().StringVar(&inputSchemaStr, "input-schema", "", "New JSON Schema for inputs")
	cmd.Flags().StringVar(&outputSchemaStr, "output-schema", "", "New JSON Schema for outputs")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func actionEnableCmd() *cobra.Command {
	var actionID string
	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Activate an action",
		RunE: func(_ *cobra.Command, _ []string) error {
			return setActionActive(actionID, true)
		},
	}
	cmd.Flags().StringVar(&actionID, "id", "", "Action ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func actionDisableCmd() *cobra.Command {
	var actionID string
	cmd := &cobra.Command{
		Use:   "disable",
		Short: "Deactivate an action",
		RunE: func(_ *cobra.Command, _ []string) error {
			return setActionActive(actionID, false)
		},
	}
	cmd.Flags().StringVar(&actionID, "id", "", "Action ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func setActionActive(actionID string, active bool) error {
	k, db, err := openKernel()
	if err != nil {
		return err
	}
	defer db.Close()

	subjectID, err := requireSubjectID(k)
	if err != nil {
		return err
	}

	if err := k.SetActive(context.Background(), subjectID, actionID, active); err != nil {
		return err
	}
	state := "disabled"
	if active {
		state = "enabled"
	}
	fmt.Printf("Action %s %s.\n", actionID, state)
	return nil
}

func actionListCmd() *cobra.Command {
	var all bool
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List actions",
		RunE: func(_ *cobra.Command, _ []string) error {
			k, db, err := openKernel()
			if err != nil {
				return err
			}
			defer db.Close()

			actions, err := k.ListActions(context.Background(), !all, limit, offset)
			if err != nil {
				return err
			}

			if flagOutput == "json" {
				return printJSON(actions)
			}
			for _, a := range actions {
				active := " "
				if a.Active {
					active = "*"
				}
				fmt.Printf("[%s] %s  %-30s  %d credits\n", active, a.ID[:8], a.Name, a.Price)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Include inactive actions")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum results")
	cmd.Flags().IntVar(&offset, "offset", 0, "Pagination offset")
	return cmd
}

func actionDeleteCmd() *cobra.Command {
	var actionID string
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete an action",
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

			if err := k.DeleteAction(context.Background(), subjectID, actionID); err != nil {
				return err
			}
			fmt.Printf("Action %s deleted.\n", actionID)
			return nil
		},
	}
	cmd.Flags().StringVar(&actionID, "id", "", "Action ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func actionACLCmd() *cobra.Command {
	aclCmd := &cobra.Command{Use: "acl", Short: "ACL management"}
	aclCmd.AddCommand(aclGrantCmd(), aclRevokeCmd())
	return aclCmd
}

func aclGrantCmd() *cobra.Command {
	var actionID, subjectHandle, perm string
	cmd := &cobra.Command{
		Use:   "grant",
		Short: "Grant a permission on an action",
		RunE: func(_ *cobra.Command, _ []string) error {
			return modifyACL(actionID, subjectHandle, kernel.Permission(perm), true)
		},
	}
	cmd.Flags().StringVar(&actionID, "action", "", "Action ID (required)")
	cmd.Flags().StringVar(&subjectHandle, "user", "", "Subject user handle (required)")
	cmd.Flags().StringVar(&perm, "perm", "call", "Permission: read, call, admin")
	_ = cmd.MarkFlagRequired("action")
	_ = cmd.MarkFlagRequired("user")
	return cmd
}

func aclRevokeCmd() *cobra.Command {
	var actionID, subjectHandle, perm string
	cmd := &cobra.Command{
		Use:   "revoke",
		Short: "Revoke a permission on an action",
		RunE: func(_ *cobra.Command, _ []string) error {
			return modifyACL(actionID, subjectHandle, kernel.Permission(perm), false)
		},
	}
	cmd.Flags().StringVar(&actionID, "action", "", "Action ID (required)")
	cmd.Flags().StringVar(&subjectHandle, "user", "", "Subject user handle (required)")
	cmd.Flags().StringVar(&perm, "perm", "call", "Permission to revoke")
	_ = cmd.MarkFlagRequired("action")
	_ = cmd.MarkFlagRequired("user")
	return cmd
}

func modifyACL(actionID, subjectHandle string, perm kernel.Permission, grant bool) error {
	k, db, err := openKernel()
	if err != nil {
		return err
	}
	defer db.Close()

	grantorID, err := requireSubjectID(k)
	if err != nil {
		return err
	}

	subject, err := k.ReadUserByHandle(context.Background(), subjectHandle)
	if err != nil {
		return fmt.Errorf("user %s not found: %w", subjectHandle, err)
	}

	if grant {
		err = k.GrantACL(context.Background(), subject.ID, actionID, perm, grantorID)
	} else {
		err = k.RevokeACL(context.Background(), subject.ID, actionID, perm, grantorID)
	}
	if err != nil {
		return err
	}
	op := "revoked"
	if grant {
		op = "granted"
	}
	fmt.Printf("Permission %s %s on %s for %s.\n", perm, op, actionID, subjectHandle)
	return nil
}

func actionGrantAllCmd() *cobra.Command {
	var actionID string
	cmd := &cobra.Command{
		Use:   "grant-all",
		Short: "Grant public (grant-all) access to an action",
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

			if err := k.GrantAll(context.Background(), subjectID, actionID); err != nil {
				return err
			}
			fmt.Printf("Action %s is now publicly callable.\n", actionID)
			return nil
		},
	}
	cmd.Flags().StringVar(&actionID, "id", "", "Action ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func actionRevokeAllCmd() *cobra.Command {
	var actionID string
	cmd := &cobra.Command{
		Use:   "revoke-all",
		Short: "Revoke public (grant-all) access from an action",
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

			if err := k.RevokeAll(context.Background(), subjectID, actionID); err != nil {
				return err
			}
			fmt.Printf("Action %s public access revoked.\n", actionID)
			return nil
		},
	}
	cmd.Flags().StringVar(&actionID, "id", "", "Action ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

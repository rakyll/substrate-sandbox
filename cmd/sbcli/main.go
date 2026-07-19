// Command sbcli is a CLI for the sandbox service. Sandbox commands go
// through the substrate-sandbox-api REST service using the sandbox SDK;
// system commands talk to the Kubernetes API.
//
// The API endpoint can be set with the --api flag or the SBCLI_API
// environment variable.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/rakyll/substrate-sandbox/sandbox"
	"github.com/spf13/cobra"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	var (
		endpoint  string
		template  string
		namespace string

		client *sandbox.Client
	)

	root := &cobra.Command{
		Use:           "sbcli",
		Short:         "Manage sandboxes on Agent Substrate",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// deploy talks to the Kubernetes API, not the sandbox API.
			if cmd.Name() == "deploy" {
				return nil
			}
			var err error
			client, err = sandbox.New(sandbox.Options{
				Endpoint:  endpoint,
				Template:  template,
				Namespace: namespace,
			})
			return err
		},
		PersistentPostRun: func(cmd *cobra.Command, args []string) {
			if client != nil {
				client.Close()
			}
		},
	}
	root.PersistentFlags().StringVar(&endpoint, "api", envOr("SBCLI_API", "http://localhost:8081"), "base URL of the substrate-sandbox-api REST service")
	root.PersistentFlags().StringVar(&template, "template", "sandbox", "ActorTemplate name (for create)")
	root.PersistentFlags().StringVar(&namespace, "namespace", "default", "Kubernetes namespace of the ActorTemplate")

	sandboxCmd := &cobra.Command{
		Use:   "sandbox",
		Short: "Manage sandbox lifecycle and run commands",
	}
	fsCmd := &cobra.Command{
		Use:   "fs",
		Short: "Operate on files and directories in a sandbox",
	}
	systemCmd := &cobra.Command{
		Use:   "system",
		Short: "Manage the system deployment",
	}
	root.AddCommand(sandboxCmd, systemCmd)
	sandboxCmd.AddCommand(fsCmd)

	systemCmd.AddCommand(newDeployCommand(&namespace, &template))

	sandboxCmd.AddCommand(&cobra.Command{
		Use:   "create <id>",
		Short: "Create and start a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sb, err := client.Create(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Printf("created %s\n", sb.ID())
			return nil
		},
	})

	sandboxCmd.AddCommand(&cobra.Command{
		Use:   "ls",
		Short: "List sandboxes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			infos, err := client.List(cmd.Context())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSTATUS\tTEMPLATE")
			for _, info := range infos {
				fmt.Fprintf(tw, "%s\t%s\t%s/%s\n", info.ID, info.Status, info.Namespace, info.TemplateName)
			}
			return tw.Flush()
		},
	})

	sandboxCmd.AddCommand(&cobra.Command{
		Use:   "info <id>",
		Short: "Show a sandbox's status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := client.Sandbox(args[0]).Info(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Printf("id:       %s\nstatus:   %s\ntemplate: %s/%s\n",
				info.ID, info.Status, info.Namespace, info.TemplateName)
			if info.WorkerPod != "" {
				fmt.Printf("worker:   %s/%s (%s)\n", info.WorkerPodNamespace, info.WorkerPod, info.WorkerPodIP)
			}
			return nil
		},
	})

	sandboxCmd.AddCommand(&cobra.Command{
		Use:   "suspend <id>",
		Short: "Snapshot to external storage and free the worker",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Sandbox(args[0]).Suspend(cmd.Context())
		},
	})

	sandboxCmd.AddCommand(&cobra.Command{
		Use:   "pause <id>",
		Short: "Snapshot locally on the node for fast resume",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Sandbox(args[0]).Pause(cmd.Context())
		},
	})

	sandboxCmd.AddCommand(&cobra.Command{
		Use:   "resume <id>",
		Short: "Resume from the latest snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Sandbox(args[0]).Resume(cmd.Context())
		},
	})

	sandboxCmd.AddCommand(&cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Sandbox(args[0]).Delete(cmd.Context())
		},
	})

	sandboxCmd.AddCommand(&cobra.Command{
		Use:   "cmd <id> <cmdline>",
		Short: "Run a shell command line in the sandbox",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := client.Sandbox(args[0]).Cmd(cmd.Context(), args[1])
			if err != nil {
				return err
			}
			os.Stdout.WriteString(res.Stdout)
			os.Stderr.WriteString(res.Stderr)
			if res.TimedOut {
				fmt.Fprintln(os.Stderr, "sbcli: command timed out")
			}
			if res.ExitCode != 0 {
				os.Exit(res.ExitCode)
			}
			return nil
		},
	})

	fsCmd.AddCommand(&cobra.Command{
		Use:   "read <id> <path>",
		Short: "Print a sandbox file to stdout",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			rc, err := client.Sandbox(args[0]).ReadFile(cmd.Context(), args[1])
			if err != nil {
				return err
			}
			defer rc.Close()
			_, err = io.Copy(os.Stdout, rc)
			return err
		},
	})

	fsCmd.AddCommand(&cobra.Command{
		Use:   "write <id> <path>",
		Short: "Write stdin to a sandbox file",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Buffer stdin: pipes are not seekable, so passing os.Stdin
			// directly would defeat the auto-resume retry.
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			return client.Sandbox(args[0]).WriteFile(cmd.Context(), args[1], bytes.NewReader(data), 0o644)
		},
	})

	fsCmd.AddCommand(&cobra.Command{
		Use:   "ls <id> <path>",
		Short: "List a sandbox directory",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := client.Sandbox(args[0]).ListDir(cmd.Context(), args[1])
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			for _, e := range entries {
				fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", e.ModeString, e.Size, e.ModTime.Format("2006-01-02 15:04"), e.Name)
			}
			return tw.Flush()
		},
	})

	fsCmd.AddCommand(&cobra.Command{
		Use:   "stat <id> <path>",
		Short: "Stat a sandbox path",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := client.Sandbox(args[0]).Stat(cmd.Context(), args[1])
			if err != nil {
				return err
			}
			fmt.Printf("path:  %s\nmode:  %s\nsize:  %d\nmtime: %s\n", e.Path, e.ModeString, e.Size, e.ModTime)
			return nil
		},
	})

	fsCmd.AddCommand(&cobra.Command{
		Use:   "rm <id> <path>",
		Short: "Delete a file or directory tree in the sandbox",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Sandbox(args[0]).Remove(cmd.Context(), args[1])
		},
	})

	fsCmd.AddCommand(&cobra.Command{
		Use:   "mkdir <id> <path>",
		Short: "Create a directory in the sandbox",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.Sandbox(args[0]).Mkdir(cmd.Context(), args[1], 0o755)
		},
	})

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "sbcli:", err)
		os.Exit(1)
	}
}

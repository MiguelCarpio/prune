package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/config"
	"github.com/gophercloud/gophercloud/v2/openstack/config/clouds"
)

const helpString = `Prune stale resources from cloud

Usage: prune [OPTION]...

Options:
  --resource-ttl=<ttl>  Minimum age of resources to prune. ttl is parsed
                        as a Go duration (e.g. "1h", "30m12s")
  --slack-hook=<hook>   Slack hook. If provided, updates will be posted to the
                        relevant Slack channel, otherwise they are dumped to
                        stdout
  --no-dry-run          Delete resources
  --help                Show this help message and exit
`

var showHelp = func() bool {
	for _, arg := range os.Args {
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}()

var bestBefore = func() time.Duration {
	for _, arg := range os.Args {
		if value := strings.TrimPrefix(arg, "--resource-ttl="); value != arg {
			d, err := time.ParseDuration(value)
			if err != nil {
				panic(err)
			}
			return d
		}
	}
	return 7 * time.Hour
}()

var dryRun = func() bool {
	for _, arg := range os.Args {
		if arg == "--no-dry-run" {
			return false
		}
	}
	return true
}()

var slackHook = func() string {
	for _, arg := range os.Args {
		if value := strings.TrimPrefix(arg, "--slack-hook="); value != arg {
			return value
		}
	}
	return ""
}()

type Resource interface {
	Dater
	Deleter
	Identifier
	Namer
	Typer
}

type Typer interface{ Type() string }
type Dater interface{ CreatedAt() time.Time }
type Deleter interface{ Delete(context.Context) error }
type Identifier interface{ ID() string }
type Namer interface{ Name() string }
type Clusterer interface{ ClusterID() string }
type Tagger interface{ Tags() []string }

// TODO:  server groups, keypairs
// TODO: volume admin setting
func main() {
	ctx := context.Background()
	if showHelp {
		fmt.Printf(helpString)
		os.Exit(0)
	}

	{
		verb := "Listing"
		if !dryRun {
			verb = "Deleting"
		}
		log.Printf("%s everything older than %s\n", verb, bestBefore)
	}
	resources := make(chan Resource)
	{
		ao, eo, tlsConfig, err := clouds.Parse()
		if err != nil {
			panic(err)
		}
		providerClient, err := config.NewProviderClient(ctx, ao, config.WithTLSConfig(tlsConfig))
		if err != nil {
			panic(err)
		}

		loadbalancerClient, err := openstack.NewLoadBalancerV2(providerClient, eo)
		if err != nil {
			// Ignore the error if Octavia is not available in the cloud
			var gerr *gophercloud.ErrEndpointNotFound
			if errors.As(err, &gerr) {
				log.Println("Skipping load balancer listing because the Octavia endpoint was not found")
			} else {
				panic(err)
			}
			loadbalancerClient = nil
		}
		computeClient, err := openstack.NewComputeV2(providerClient, eo)
		if err != nil {
			panic(err)
		} else {
			// Required for server tags
			computeClient.Microversion = "2.26"
		}
		networkClient, err := openstack.NewNetworkV2(providerClient, eo)
		if err != nil {
			panic(err)
		}
		volumeClient, err := openstack.NewBlockStorageV3(providerClient, eo)
		if err != nil {
			panic(err)
		}
		identityClient, err := openstack.NewIdentityV3(providerClient, eo)
		if err != nil {
			panic(err)
		}
		imageClient, err := openstack.NewImageV2(providerClient, eo)
		if err != nil {
			panic(err)
		}

		containerClient, err := openstack.NewContainerV1(providerClient, eo)
		if err != nil {
			// Ignore the error if Swift is not available in the cloud
			var gerr *gophercloud.ErrEndpointNotFound
			if errors.As(err, &gerr) {
				log.Println("Skipping container listing because the Swift endpoint was not found")
			} else {
				panic(err)
			}
			containerClient = nil
		}

		shareClient, err := openstack.NewSharedFileSystemV2(providerClient, eo)
		if err != nil {
			// Ignore the error if Manila is not available in the cloud
			var gerr *gophercloud.ErrEndpointNotFound
			if errors.As(err, &gerr) {
				log.Println("Skipping share listing because the Manila endpoint was not found")
			} else {
				panic(err)
			}
			shareClient = nil
		}

		go func() {
			defer close(resources)

			for res := range ListFloatingIPs(ctx, networkClient) {
				resources <- res
			}

			if loadbalancerClient != nil {
				for res := range ListLoadBalancers(ctx, loadbalancerClient) {
					resources <- res
				}
			}

			for res := range Filter(ListServers(ctx, computeClient), NameIsNot[Resource]("metrics")) {
				resources <- res
			}

			for res := range Filter(ListRouters(ctx, networkClient), NameIsNot[Resource]("dualstack")) {
				resources <- res
			}

			for res := range ListTrunks(ctx, networkClient) {
				resources <- res
			}

			for res := range ListPorts(ctx, networkClient) {
				resources <- res
			}

			for res := range Filter(ListNetworks(ctx, networkClient), NameDoesNotContain[Resource]("hostonly", "external", "sahara-access", "mellanox", "intel", "public", "provider")) {
				resources <- res
			}

			for res := range ListVolumeSnapshots(ctx, volumeClient) {
				resources <- res
			}

			for res := range ListVolumes(ctx, volumeClient) {
				resources <- res
			}

			for res := range Filter(ListSecurityGroups(ctx, networkClient), NameIsNot[Resource]("default", "ssh", "allow_ssh", "allow_ping")) {
				resources <- res
			}

			if shareClient != nil {
				for res := range ListShares(ctx, shareClient) {
					resources <- res
				}
			}

			for res := range ListPerishableApplicationCredentials(ctx, identityClient) {
				resources <- res
			}

			if containerClient != nil {
				for res := range Filter(ListContainers(ctx, containerClient, ListNetworks(ctx, networkClient)), NameIsNot[Resource]("shiftstack-metrics", "shiftstack-bot")) {
					resources <- res
				}
			}

			for res := range Filter(ListImages(ctx, imageClient), NameMatchesOneOfThesePatterns[Resource](".{8}-.{5}-.{5}-ignition", ".{8}-.{5}-.{5}-rhcos", "bootstrap-ign-.{8}-.{5}-.{5}", "rhcos-.{7,8}-.{5}")) {
				resources <- res
			}
		}()
	}

	now := time.Now()
	report := Report{Time: now}
	for staleResource := range Filter(resources, TagsDoNotContain("shiftstack-prune=keep"), CreatedBefore[Resource](now.Add(-bestBefore))) {
		report.AddFound(staleResource)

		if !dryRun {
			log.Printf("Deleting %s %q (created at %s)...\n", staleResource.Type(), staleResource.ID(), staleResource.CreatedAt().Format(time.RFC3339))
			if err := staleResource.Delete(ctx); err != nil {
				log.Printf("error deleting %s %q: %v\n", staleResource.Type(), staleResource.ID(), err)
				report.AddFailedToDelete(staleResource)
			} else {
				log.Printf("deleted %s %q\n", staleResource.Type(), staleResource.ID())
				report.AddDeleted(staleResource)
			}
		}
	}

	encoder := json.NewEncoder(os.Stdout)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		encoder.SetIndent("", "  ")
	}

	if err := encoder.Encode(report); err != nil {
		panic(err)
	}

	if len(report.FailedToDelete) > 0 && slackHook != "" {
		log.Printf("Sending failed_to_delete report to Slack")
		if err := reportToSlack(slackHook, report); err != nil {
			log.Fatalf("Failed to send a report to Slack: %v", err)
		}
	}
}

func init() {
	log.SetFlags(log.Ldate | log.Ltime | log.LUTC)
}

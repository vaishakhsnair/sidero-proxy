package watcher

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"sidero-proxy/internal/nat"
)

type EndpointResolver interface {
	Resolve(ctx context.Context) (ResolvedState, error)
	Events(ctx context.Context) (<-chan struct{}, <-chan error)
}

type ResolvedState struct {
	Destinations  map[int]nat.DNATDestination
	BridgeSubnets []string
}

type DockerResolver struct {
	publicIP      string
	dockerNetwork string
	startPort     int
	endPort       int
}

type dockerInspect struct {
	ID              string `json:"Id"`
	Name            string `json:"Name"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
		Ports map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
	} `json:"NetworkSettings"`
}

type dockerNetworkInspect struct {
	Name string `json:"Name"`
	IPAM struct {
		Config []struct {
			Subnet string `json:"Subnet"`
		} `json:"Config"`
	} `json:"IPAM"`
}

func NewDockerResolver(publicIP, dockerNetwork string, startPort, endPort int) *DockerResolver {
	return &DockerResolver{
		publicIP:      publicIP,
		dockerNetwork: dockerNetwork,
		startPort:     startPort,
		endPort:       endPort,
	}
}

func (r *DockerResolver) Resolve(ctx context.Context) (ResolvedState, error) {
	ids, err := r.containerIDs(ctx)
	if err != nil {
		return ResolvedState{}, err
	}
	if len(ids) == 0 {
		return ResolvedState{Destinations: map[int]nat.DNATDestination{}}, nil
	}

	args := append([]string{"inspect"}, ids...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.Output()
	if err != nil {
		return ResolvedState{}, fmt.Errorf("docker inspect: %w", err)
	}

	var containers []dockerInspect
	if err := json.Unmarshal(out, &containers); err != nil {
		return ResolvedState{}, fmt.Errorf("decode docker inspect output: %w", err)
	}

	destinations := make(map[int]nat.DNATDestination)
	networkNames := make(map[string]struct{})
	for _, container := range containers {
		networkName, ip := r.containerEndpoint(container)
		if ip == "" {
			continue
		}
		if networkName != "" {
			networkNames[networkName] = struct{}{}
		}
		for spec, bindings := range container.NetworkSettings.Ports {
			containerPort, proto, ok := strings.Cut(spec, "/")
			if !ok || proto != "tcp" {
				continue
			}
			portValue, err := strconv.Atoi(containerPort)
			if err != nil {
				continue
			}
			for _, binding := range bindings {
				if binding.HostIP != r.publicIP {
					continue
				}
				hostPort, err := strconv.Atoi(binding.HostPort)
				if err != nil {
					continue
				}
				if hostPort < r.startPort || hostPort > r.endPort {
					continue
				}
				destinations[hostPort] = nat.DNATDestination{
					IP:   ip,
					Port: portValue,
				}
			}
		}
	}

	subnets, err := r.networkSubnets(ctx, networkNames)
	if err != nil {
		return ResolvedState{}, err
	}
	return ResolvedState{
		Destinations:  destinations,
		BridgeSubnets: subnets,
	}, nil
}

func (r *DockerResolver) Events(ctx context.Context) (<-chan struct{}, <-chan error) {
	triggerCh := make(chan struct{}, 1)
	errCh := make(chan error, 1)

	go func() {
		defer close(triggerCh)
		defer close(errCh)

		cmd := exec.CommandContext(ctx, "docker", "events", "--format", "{{json .}}")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			errCh <- fmt.Errorf("docker events stdout: %w", err)
			return
		}
		if err := cmd.Start(); err != nil {
			errCh <- fmt.Errorf("start docker events: %w", err)
			return
		}

		dec := json.NewDecoder(stdout)
		type event struct {
			Type   string `json:"Type"`
			Action string `json:"Action"`
		}
		relevant := map[string]struct{}{
			"start":                    {},
			"stop":                     {},
			"die":                      {},
			"restart":                  {},
			"connect":                  {},
			"disconnect":               {},
			"health_status: healthy":   {},
			"health_status: unhealthy": {},
		}

		for {
			var e event
			if err := dec.Decode(&e); err != nil {
				if ctx.Err() != nil {
					return
				}
				errCh <- fmt.Errorf("decode docker event: %w", err)
				return
			}
			if e.Type != "container" {
				continue
			}
			if _, ok := relevant[e.Action]; !ok {
				continue
			}
			select {
			case triggerCh <- struct{}{}:
			default:
			}
		}
	}()

	return triggerCh, errCh
}

func (r *DockerResolver) containerIDs(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, "docker", "ps", "--quiet")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	lines := strings.Fields(string(out))
	sort.Strings(lines)
	return lines, nil
}

func (r *DockerResolver) containerEndpoint(container dockerInspect) (string, string) {
	if r.dockerNetwork != "" {
		if network, ok := container.NetworkSettings.Networks[r.dockerNetwork]; ok && network.IPAddress != "" {
			return r.dockerNetwork, network.IPAddress
		}
	}
	networkNames := make([]string, 0, len(container.NetworkSettings.Networks))
	for name := range container.NetworkSettings.Networks {
		networkNames = append(networkNames, name)
	}
	sort.Strings(networkNames)
	for _, name := range networkNames {
		network := container.NetworkSettings.Networks[name]
		if network.IPAddress != "" {
			return name, network.IPAddress
		}
	}
	return "", ""
}

func (r *DockerResolver) networkSubnets(ctx context.Context, networkNames map[string]struct{}) ([]string, error) {
	if len(networkNames) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(networkNames))
	for name := range networkNames {
		names = append(names, name)
	}
	sort.Strings(names)
	args := append([]string{"network", "inspect"}, names...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker network inspect: %w", err)
	}
	var networks []dockerNetworkInspect
	if err := json.Unmarshal(out, &networks); err != nil {
		return nil, fmt.Errorf("decode docker network inspect output: %w", err)
	}
	var subnets []string
	for _, network := range networks {
		for _, cfg := range network.IPAM.Config {
			if subnet := strings.TrimSpace(cfg.Subnet); subnet != "" && !strings.Contains(subnet, ":") {
				subnets = append(subnets, subnet)
			}
		}
	}
	sort.Strings(subnets)
	return subnets, nil
}

package types

// ContainerService represents a parsed container with its Tailscale service configuration
type ContainerService struct {
	ContainerID      string
	ContainerName    string
	ServiceName      string
	Port             string   // Tailscale service port (e.g., "443")
	TargetPort       string   // Container/host port to proxy to (e.g., "9080")
	ServiceProtocol  string   // Protocol Tailscale uses (e.g., "https", "http", "tcp")
	Protocol         string   // Protocol the container speaks (e.g., "http", "https", "tcp")
	Tags             []string // Tailscale service tags (e.g., ["tag:container", "tag:web"])
	IPAddress        string
	FunnelEnabled    bool   // Enable Tailscale Funnel (public internet access)
	FunnelPort       string // Container port for funnel (separate from service port)
	FunnelTargetPort string // Host port that maps to FunnelPort
	FunnelFunnelPort string // Public-facing port (443, 8443, or 10000 for HTTPS)
	FunnelProtocol   string // Funnel protocol (https, tcp, tls-terminated-tcp)
}

// TailscaleServiceConfig represents the JSON structure for Tailscale service configuration
type TailscaleServiceConfig struct {
	Version  string                       `json:"version"`
	Services map[string]ServiceDefinition `json:"services"`
}

// ServiceDefinition defines a single Tailscale service
type ServiceDefinition struct {
	Endpoints map[string]string `json:"endpoints"`
}

// Labels for container discovery
const (
	LabelEnable           = "docktail.service.enable"
	LabelService          = "docktail.service.name"
	LabelPort             = "docktail.service.service-port"
	LabelServiceProtocol  = "docktail.service.service-protocol"
	LabelTarget           = "docktail.service.port"
	LabelTargetProtocol   = "docktail.service.protocol"
	LabelTags             = "docktail.tags"
	LabelFunnelEnable     = "docktail.funnel.enable"
	LabelFunnelPort       = "docktail.funnel.port"        // Container port (like service.port)
	LabelFunnelFunnelPort = "docktail.funnel.funnel-port" // Public port (443, 8443, 10000)
	LabelFunnelProtocol   = "docktail.funnel.protocol"
	LabelDirect           = "docktail.service.direct"  // Direct container IP proxying (default: true, set to "false" to use published ports)
	LabelNetwork          = "docktail.service.network" // Docker network to use for container IP (default: bridge or first available)
)

package types

// ContainerService represents a parsed container with its Tailscale service configuration
type ContainerService struct {
	ContainerID        string
	ContainerName      string
	ServiceEnabled     bool
	ServiceName        string
	ServiceDescription string   // Human-readable Service description synced to the Tailscale Control Plane (admin panel "comment")
	Port               string   // Tailscale service port (e.g., "443")
	TargetPort         string   // Container/host port to proxy to (e.g., "9080")
	ServiceProtocol    string   // Protocol Tailscale uses (e.g., "https", "http", "tcp")
	Protocol           string   // Protocol the container speaks (e.g., "http", "https", "tcp")
	Tags               []string // Tailscale service tags (e.g., ["tag:container", "tag:web"])
	IPAddress          string
	// MonitorIP/MonitorPort are where the LOCAL health check should probe, set only
	// when it must diverge from IPAddress/TargetPort (the tailscale-serve
	// destination, which is resolved for tailscaled in the host netns and so can be
	// a host-relative 127.0.0.1 the agent can't reach from inside its own
	// container):
	//   - published-port mode: the container's own network IP and container port;
	//   - host-network mode:   the docker host gateway and the container port.
	// Empty for direct mode (IPAddress is already the container IP), when the agent
	// itself shares the host netns (127.0.0.1 works), or when nothing is resolvable.
	MonitorIP   string
	MonitorPort string
	// ProxyProtocol is the PROXY protocol version Tailscale should prepend on
	// TCP forwards (1 or 2). Zero means the header is not sent. Only valid
	// with service-protocol tcp or tls-terminated-tcp; HTTP/HTTPS
	// service-protocol must not set this, or Tailscale serve rejects the config.
	ProxyProtocol    int
	FunnelEnabled    bool   // Enable Tailscale Funnel (public internet access)
	FunnelPort       string // Container port for funnel (separate from service port)
	FunnelTargetPort string // Host port that maps to FunnelPort
	FunnelFunnelPort string // Public-facing port (443, 8443, or 10000 for HTTPS)
	FunnelProtocol   string // Funnel protocol (https, tcp, tls-terminated-tcp)
	FunnelPath       string // HTTP(S) Funnel path (for example "/" or "/webhook")
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
	LabelDescription      = "docktail.service.description"
	LabelPort             = "docktail.service.service-port"
	LabelServiceProtocol  = "docktail.service.service-protocol"
	LabelTarget           = "docktail.service.port"
	LabelTargetProtocol   = "docktail.service.protocol"
	LabelProxyProtocol    = "docktail.service.proxy-protocol"
	LabelTags             = "docktail.tags"
	LabelFunnelEnable     = "docktail.funnel.enable"
	LabelFunnelPort       = "docktail.funnel.port"        // Container port (like service.port)
	LabelFunnelFunnelPort = "docktail.funnel.funnel-port" // Public port (443, 8443, 10000)
	LabelFunnelProtocol   = "docktail.funnel.protocol"
	LabelFunnelPath       = "docktail.funnel.path"
	LabelDirect           = "docktail.service.direct"  // Direct container IP proxying (default: true, set to "false" to use published ports)
	LabelNetwork          = "docktail.service.network" // Docker network to use for container IP (default: bridge or first available)
)

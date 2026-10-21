// Package model holds the types every anastasis package shares.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Project is a fully resolved Compose project as anastasis runs it.
type Project struct {
	Name     string             `json:"name"`
	Services map[string]Service `json:"services"`
	Volumes  []string           `json:"volumes,omitempty"` // full names of named volumes
}

// Service is one Compose service plus the settings anastasis derives from it.
type Service struct {
	Project   string        `json:"project"`
	Name      string        `json:"name"`
	Hash      string        `json:"hash"`
	Replicas  int           `json:"replicas"`
	Update    UpdateConfig  `json:"update"`
	Expose    *Expose       `json:"expose,omitempty"`
	Sticky    bool          `json:"sticky,omitempty"`
	Autoscale *Autoscale    `json:"autoscale,omitempty"`
	StopGrace time.Duration `json:"stopGrace"`
	Config    Config        `json:"config"`
}

// UpdateConfig is deploy.update_config with anastasis's defaults applied.
type UpdateConfig struct {
	Parallelism   int           `json:"parallelism"`
	Delay         time.Duration `json:"delay"`
	Monitor       time.Duration `json:"monitor"`
	Order         string        `json:"order"`         // "start-first" or "stop-first"
	FailureAction string        `json:"failureAction"` // "pause", "rollback" or "continue"
}

// Expose is x-anastasis.expose: the load balancer listens on HostPort and
// forwards to ContainerPort.
type Expose struct {
	HostPort      int `json:"hostPort"`
	ContainerPort int `json:"containerPort"`
}

// Autoscale is x-anastasis.autoscale: bounds and targets for the autoscaler.
// A zero Inflight or CPU target turns that signal off.
type Autoscale struct {
	Min      int     `json:"min"`
	Max      int     `json:"max"`
	Inflight float64 `json:"inflight,omitempty"` // concurrent requests per ready replica
	CPU      float64 `json:"cpu,omitempty"`      // percent of CPULimit per ready replica
	CPULimit float64 `json:"cpuLimit,omitempty"` // cores, from deploy.resources.limits.cpus
}

// Replica is one container of a service as observed in the runtime.
type Replica struct {
	ID       string
	Name     string
	Service  string
	Hash     string
	State    string // "running", "exited", "created", ...
	Health   string // "healthy", "unhealthy", "starting", "stalled" (check overdue), or "" without a healthcheck
	Started  time.Time
	Finished time.Time
	HostPort int // loopback port of the exposed container port, 0 if none
}

// Dead reports whether the replica must be removed.
func (r Replica) Dead() bool { return r.State != "running" || r.Health == "unhealthy" }

// Ready reports whether the replica can receive traffic.
func (r Replica) Ready() bool {
	return r.State == "running" && (r.Health == "healthy" || r.Health == "")
}

// ConfigHash hashes the parts of a service that end up in its containers.
// Replica count, update settings and stop grace live outside Config, so
// changing them never triggers a rollout. Of Expose only the container port
// reaches the containers; the host port is the load balancer's.
func ConfigHash(svc Service) string {
	expose := svc.Expose
	if expose != nil {
		expose = &Expose{ContainerPort: expose.ContainerPort}
	}
	b, err := json.Marshal(struct {
		Config Config
		Expose *Expose
	}{svc.Config, expose})
	if err != nil {
		panic(err) // plain data, always marshals
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:6])
}

// WithSalt mixes a rollout salt into a config hash.
func WithSalt(hash, salt string) string {
	if salt == "" {
		return hash
	}
	sum := sha256.Sum256([]byte(hash + "/" + salt))
	return hex.EncodeToString(sum[:6])
}

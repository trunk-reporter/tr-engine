package api

import (
	"net"
	"os"
	"runtime"
)

// collectEnvironment gathers server environment information for debug reports.
func collectEnvironment() map[string]any {
	env := map[string]any{
		"go_version": runtime.Version(),
		"go_os":      runtime.GOOS,
		"go_arch":    runtime.GOARCH,
		"num_cpu":    runtime.NumCPU(),
	}

	if hostname, err := os.Hostname(); err == nil {
		env["hostname"] = hostname
	}

	_, err := os.Stat("/.dockerenv")
	env["in_container"] = err == nil

	ifaces, err := net.Interfaces()
	if err == nil {
		var interfaces []map[string]any
		for _, iface := range ifaces {
			if iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil || len(addrs) == 0 {
				continue
			}
			addrStrings := make([]string, len(addrs))
			for i, addr := range addrs {
				addrStrings[i] = addr.String()
			}
			interfaces = append(interfaces, map[string]any{
				"name":  iface.Name,
				"flags": iface.Flags.String(),
				"addrs": addrStrings,
			})
		}
		env["network_interfaces"] = interfaces
	}

	return env
}

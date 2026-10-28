package agent

import (
	"fmt"
	"strings"
)

// chain is the per-VM iptables chain holding its egress rules.
func chain(vm *VM) string { return fmt.Sprintf("DLY-%d", vm.Index) }

func subnet(vm *VM) string {
	ip := vm.HostIP[:strings.LastIndex(vm.HostIP, ".")+1]
	last := vm.HostIP[strings.LastIndex(vm.HostIP, ".")+1:]
	var n int
	fmt.Sscanf(last, "%d", &n)
	return fmt.Sprintf("%s%d/30", ip, n-1)
}

// networkRules returns the setup commands for a VM. The VM may reach neither
// the node (INPUT) nor other VMs (FORWARD to taps), and may reach only
// public destinations outside DenyCIDRs when egress is allowed.
func (m *Manager) networkRules(vm *VM) [][]string {
	c := chain(vm)
	cmds := [][]string{
		{"ip", "tuntap", "add", "dev", vm.Tap, "mode", "tap"},
		{"ip", "addr", "add", vm.HostIP + "/30", "dev", vm.Tap},
		{"ip", "link", "set", vm.Tap, "up"},
		{"iptables", "-I", "INPUT", "-i", vm.Tap, "-j", "DROP"},
		{"iptables", "-I", "FORWARD", "-o", vm.Tap, "-j", "DROP"},
		{"iptables", "-I", "FORWARD", "-o", vm.Tap, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
		{"iptables", "-N", c},
		{"iptables", "-I", "FORWARD", "-i", vm.Tap, "-j", c},
	}
	for _, cidr := range m.cfg.DenyCIDRs {
		cmds = append(cmds, []string{"iptables", "-A", c, "-d", cidr, "-j", "DROP"})
	}
	if m.cfg.Egress == "allow" {
		cmds = append(cmds,
			[]string{"iptables", "-A", c, "-j", "ACCEPT"},
			[]string{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", subnet(vm), "-j", "MASQUERADE"})
	} else {
		cmds = append(cmds, []string{"iptables", "-A", c, "-j", "DROP"})
	}
	return cmds
}

func (m *Manager) setupNetwork(vm *VM) error {
	m.teardownNetwork(vm) // clear leftovers from a crashed attempt
	for _, cmd := range m.networkRules(vm) {
		if err := m.runner.Run(cmd[0], cmd[1:]...); err != nil {
			return err
		}
	}
	return nil
}

// teardownNetwork removes everything setupNetwork created. Errors are
// ignored: rules may already be gone.
func (m *Manager) teardownNetwork(vm *VM) {
	c := chain(vm)
	for _, cmd := range [][]string{
		{"iptables", "-D", "INPUT", "-i", vm.Tap, "-j", "DROP"},
		{"iptables", "-D", "FORWARD", "-o", vm.Tap, "-j", "DROP"},
		{"iptables", "-D", "FORWARD", "-o", vm.Tap, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
		{"iptables", "-D", "FORWARD", "-i", vm.Tap, "-j", c},
		{"iptables", "-F", c},
		{"iptables", "-X", c},
		{"iptables", "-t", "nat", "-D", "POSTROUTING", "-s", subnet(vm), "-j", "MASQUERADE"},
		{"ip", "link", "del", vm.Tap},
	} {
		_ = m.runner.Run(cmd[0], cmd[1:]...)
	}
}

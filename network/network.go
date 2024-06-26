package network

import (
	"bytes"
	"context"
	"dnshook/pkg/config"
	"dnshook/pkg/shutdown"
	"fmt"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"log"
	"os/exec"
	"strings"
	"sync"
	"text/template"
	"time"
)

const tableTmp = `
table ip vpn_manager {

    set no_vpn_domain_ip_set {
        type ipv4_addr;
    }

    set no_vpn_ip_set {
        type ipv4_addr;flags interval;
    }

    chain prerouting {
        type filter hook prerouting priority 0;
        {{.}}
    }

    chain select_export {
        ip daddr @no_vpn_ip_set return
        ip daddr @no_vpn_domain_ip_set return
        jump vpn
    }

    chain vpn {
        reject
    }

}
`

type Interface struct {
	Name   string `yaml:"name"`
	Weight int    `yaml:"weight"`
	Mark   string `yaml:"mark"`
}

type Config struct {
	VpnInterfaces      []Interface `yaml:"vpn-interfaces"`
	LanInterfaces      []string    `yaml:"lan-interfaces"`
	NoVpnIps           []string    `yaml:"no-vpn-ips"`
	PingAddresses      []string    `yaml:"ping-addresses"`
	PingTimeoutSeconds int         `yaml:"ping-timeout-seconds"`
}

var defaultConfig = Config{
	VpnInterfaces: []Interface{
		{Name: "vpn", Weight: 1, Mark: "0x3e9"},
	},
	LanInterfaces:      []string{"br-lan"},
	NoVpnIps:           []string{"192.168.0.0/16"},
	PingAddresses:      []string{"8.8.8.8", "cloudflare.com"},
	PingTimeoutSeconds: 4,
}

var (
	vpnInterfaces     = map[string]*ethernet{}
	cancel            func()
	conf              *Config
	getNoVpnDomainIps func() []string
)

const configFileName = "/etc/vpnmanager/config.yml"

func Start(g func() []string) error {
	getNoVpnDomainIps = g
	if err := start(); err != nil {
		return err
	}
	shutdown.OnShutdown(func(ctx context.Context) error {
		return clearAll()
	})
	return nil
}

func start() error {
	if conf == nil {
		c := config.LocalYamlConfig[Config](configFileName, defaultConfig)
		cf := c.Get()
		conf = &cf
		if err := c.Watch(func(c Config) {
			logrus.Info("config changed")
			conf = &c
			if err := clearAll(); err != nil {
				logrus.WithError(err).Error("clear all failed")
				return
			}
			if err := start(); err != nil {
				logrus.WithError(err).Error("start failed")
			}
		}); err != nil {
			return err
		}
	}
	var v string
	if len(conf.LanInterfaces) == 1 {
		v = fmt.Sprintf("iifname %s jump select_export", conf.LanInterfaces[0])
	} else if len(conf.LanInterfaces) > 1 {
		v = fmt.Sprintf("iifname { %s } jump select_export", strings.Join(conf.LanInterfaces, ","))
	}
	pingTimeout := time.Duration(conf.PingTimeoutSeconds) * time.Second
	//初始化两个map
	for _, vpnIf := range conf.VpnInterfaces {
		e := &ethernet{
			Interface:       vpnIf,
			pingTimeout:     pingTimeout,
			pingAddr:        conf.PingAddresses,
			onStatusChanged: setVpnChainRules,
		}
		vpnInterfaces[vpnIf.Name] = e
	}

	//创建nftable
	tmp, err := template.New("table").Parse(tableTmp)
	if err != nil {
		return errors.WithStack(err)
	}
	var buf bytes.Buffer
	err = tmp.Execute(&buf, v)
	if err != nil {
		return errors.WithStack(err)
	}
	log.Println(buf.String())
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(buf.String())
	if err = runCmd(cmd); err != nil {
		return err
	}

	if len(conf.NoVpnIps) > 0 {
		cmd = exec.Command("nft", "add", "element", "ip", "vpn_manager", "no_vpn_ip_set", fmt.Sprintf("{ %s }", strings.Join(conf.NoVpnIps, ",")))
		if err = runCmd(cmd); err != nil {
			return err
		}
	}

	//初始化nft规则
	ctx, c := context.WithCancel(context.Background())
	cancel = c
	wg := sync.WaitGroup{}
	for _, e := range vpnInterfaces {
		wg.Add(1)
		e := e
		go func() {
			e.keepCheck(ctx)
			wg.Done()
		}()
	}
	wg.Wait()
	if err = setVpnChainRules(); err != nil {
		log.Printf("set vpn chain rule error : %+v", err)
	}
	ips := getNoVpnDomainIps()
	if len(ips) > 0 {
		if err = AddNoVpnDomainIp(ips...); err != nil {
			logrus.WithError(err).Error("add no vpn domain ip failed")
		}
	}
	return nil
}

var clearIprouteCommands []*exec.Cmd

func clearRouteRules() error {
	for _, command := range clearIprouteCommands {
		if err := runCmd(command); err != nil {
			log.Printf("clear ip route rule error %+v\n", err)
			continue
		}
	}
	clearIprouteCommands = nil
	return nil
}

func AddNoVpnDomainIp(ips ...string) error {
	if len(ips) == 0 {
		return nil
	}
	cmd := exec.Command("nft", "add", "element", "ip", "vpn_manager", "no_vpn_domain_ip_set", fmt.Sprintf("{ %s }", strings.Join(ips, ",")))
	return runCmd(cmd)
}

func DelNoVpnDomainIp(ips ...string) error {
	if len(ips) == 0 {
		return nil
	}
	cmd := exec.Command("nft", "delete", "element", "ip", "vpn_manager", "no_vpn_domain_ip_set", fmt.Sprintf("{ %s }", strings.Join(ips, ",")))
	return runCmd(cmd)
}

func FlushNoVpnDomainIp() error {
	cmd := exec.Command("nft", "flush", "set", "ip", "vpn_manager", "no_vpn_domain_ip_set")
	return runCmd(cmd)
}

func setVpnChainRules() error {
	//clear
	{
		cmd := exec.Command("nft", "flush", "chain", "ip", "vpn_manager", "vpn")
		if err := runCmd(cmd); err != nil {
			return err
		}
	}
	var list []*ethernet
	total := 0
	for _, e := range vpnInterfaces {
		if e.status == available {
			list = append(list, e)
			weight := e.Weight
			if weight < 1 {
				weight = 1
			}
			total += weight
		}
	}
	if len(list) == 0 {
		cmd := exec.Command("nft", "add", "rule", "ip", "vpn_manager", "vpn", "reject")
		return runCmd(cmd)
	}
	if len(list) == 1 {
		cmd := exec.Command("nft", "add", "rule", "ip", "vpn_manager", "vpn", "meta", "mark", "set", list[0].Mark)
		return runCmd(cmd)
	}

	current := 0
	betweenArr := make([]string, len(list))
	for i, s := range list {
		start := current
		end := 100
		weight := s.Weight
		if weight < 1 {
			weight = 1
		}
		if i+1 < len(list) {
			end = int((float64(weight) / float64(total)) * 100)
		}
		current = end + 1
		betweenArr[i] = fmt.Sprintf("%d-%d : %s", start, end, s.Mark)
	}
	between := fmt.Sprintf("{ %s }", strings.Join(betweenArr, ","))
	cmd := exec.Command("nft", "add", "rule", "ip", "vpn_manager", "vpn", "ct", "state", "established,related", "meta", "mark", "set", "ct", "mark")
	if err := runCmd(cmd); err != nil {
		return err
	}
	cmd = exec.Command("nft", "add", "rule", "ip", "vpn_manager", "vpn", "ct", "state", "new", "meta", "mark", "set", "numgen", "inc", "mod", "100", "map", between)
	return runCmd(cmd)
}

func clearAll() error {
	cancel()
	if err := clearRouteRules(); err != nil {
		return err
	}
	cmd := exec.Command("nft", "delete", "table", "ip", "vpn_manager")
	return runCmd(cmd)
}

func runCmd(cmd *exec.Cmd) error {
	output, err := cmd.CombinedOutput() // 获取命令的输出和错误
	if err != nil {
		return errors.Wrapf(err, "failed to execute cmd '%s', output: %s", cmd.String(), string(output))
	}
	return nil
}

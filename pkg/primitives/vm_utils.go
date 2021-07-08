package primitives

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
	"github.com/threefoldtech/zos/pkg"
	"github.com/threefoldtech/zos/pkg/gridtypes"
	"github.com/threefoldtech/zos/pkg/gridtypes/zos"
	"github.com/threefoldtech/zos/pkg/network/ifaceutil"
	"github.com/threefoldtech/zos/pkg/stubs"
)

func (p *Primitives) newYggNetworkInterface(ctx context.Context, wl *gridtypes.WorkloadWithID) (pkg.VMIface, error) {
	network := stubs.NewNetworkerStub(p.zbus)

	//TODO: if we use `ygg` as a network name. this will conflict
	//if the user has a network that is called `ygg`.
	tapName := tapNameFromName(wl.ID, "ygg")
	iface, err := network.SetupYggTap(ctx, tapName)
	if err != nil {
		return pkg.VMIface{}, errors.Wrap(err, "could not set up tap device")
	}

	out := pkg.VMIface{
		Tap: iface.Name,
		MAC: iface.HW.String(),
		IPs: []net.IPNet{
			iface.IP,
		},
		Routes: []pkg.Route{
			{
				Net: net.IPNet{
					IP:   net.ParseIP("200::"),
					Mask: net.CIDRMask(7, 128),
				},
				Gateway: iface.Gateway.IP,
			},
		},
		Public: false,
	}

	return out, nil
}

func (p *Primitives) newPrivNetworkInterface(ctx context.Context, dl gridtypes.Deployment, wl *gridtypes.WorkloadWithID, inf zos.MachineInterface) (pkg.VMIface, error) {
	network := stubs.NewNetworkerStub(p.zbus)
	netID := zos.NetworkID(dl.TwinID, inf.Network)

	subnet, err := network.GetSubnet(ctx, netID)
	if err != nil {
		return pkg.VMIface{}, errors.Wrapf(err, "could not get network resource subnet")
	}

	if !subnet.Contains(inf.IP) {
		return pkg.VMIface{}, fmt.Errorf("IP %s is not part of local nr subnet %s", inf.IP.String(), subnet.String())
	}

	privNet, err := network.GetNet(ctx, netID)
	if err != nil {
		return pkg.VMIface{}, errors.Wrapf(err, "could not get network range")
	}

	addrCIDR := net.IPNet{
		IP:   inf.IP,
		Mask: subnet.Mask,
	}

	gw4, gw6, err := network.GetDefaultGwIP(ctx, netID)
	if err != nil {
		return pkg.VMIface{}, errors.Wrap(err, "could not get network resource default gateway")
	}

	privIP6, err := network.GetIPv6From4(ctx, netID, inf.IP)
	if err != nil {
		return pkg.VMIface{}, errors.Wrap(err, "could not convert private ipv4 to ipv6")
	}

	tapName := tapNameFromName(wl.ID, string(inf.Network))
	iface, err := network.SetupPrivTap(ctx, netID, tapName)
	if err != nil {
		return pkg.VMIface{}, errors.Wrap(err, "could not set up tap device")
	}

	out := pkg.VMIface{
		Tap: iface,
		MAC: "", // rely on static IP configuration so we don't care here
		IPs: []net.IPNet{
			addrCIDR, privIP6,
		},
		Routes: []pkg.Route{
			{Net: privNet, Gateway: gw4},
		},
		IP4DefaultGateway: net.IP(gw4),
		IP6DefaultGateway: gw6,
		Public:            false,
	}

	return out, nil
}

func (p *Primitives) newPubNetworkInterface(ctx context.Context, deployment gridtypes.Deployment, cfg ZMachine) (pkg.VMIface, error) {
	network := stubs.NewNetworkerStub(p.zbus)
	ipWl, err := deployment.Get(cfg.Network.PublicIP)
	if err != nil {
		return pkg.VMIface{}, err
	}
	name := ipWl.ID.String()

	pubIP, pubGw, err := p.getPubIPConfig(ipWl)
	if err != nil {
		return pkg.VMIface{}, errors.Wrap(err, "could not get public ip config")
	}

	pubIface, err := network.SetupPubTap(ctx, name)
	if err != nil {
		return pkg.VMIface{}, errors.Wrap(err, "could not set up tap device for public network")
	}

	// the mac address uses the global workload id
	// this needs to be the same as how we get it in the actual IP reservation
	mac := ifaceutil.HardwareAddrFromInputBytes([]byte(ipWl.ID.String()))

	return pkg.VMIface{
		Tap: pubIface,
		MAC: mac.String(), // mac so we always get the same IPv6 from slaac
		IPs: []net.IPNet{
			pubIP,
		},
		IP4DefaultGateway: pubGw,
		// for now we get ipv6 from slaac, so leave ipv6 stuffs this empty
		Public: true,
	}, nil
}

// Get the public ip, and the gateway from the reservation ID
func (p *Primitives) getPubIPConfig(wl *gridtypes.WorkloadWithID) (ip net.IPNet, gw net.IP, err error) {

	//CRITICAL: TODO
	// in this function we need to return the IP from the IP workload
	// but we also need to get the Gateway IP from the farmer some how
	// we used to get this from the explorer, but now we need another
	// way to do this. for now the only option is to get it from the
	// reservation itself. hence we added the gatway fields to ip data
	if wl.Type != zos.PublicIPType {
		return ip, gw, fmt.Errorf("workload for public IP is of wrong type")
	}

	if wl.Result.State != gridtypes.StateOk {
		return ip, gw, fmt.Errorf("public ip workload is not okay")
	}
	ipData, err := wl.WorkloadData()
	if err != nil {
		return
	}
	data, ok := ipData.(*zos.PublicIP)
	if !ok {
		return ip, gw, fmt.Errorf("invalid ip data in deployment got '%T'", ipData)
	}

	return data.IP.IPNet, data.Gateway, nil
}

func getFlistInfo(imagePath string) (FListInfo, error) {
	// entities, err := ioutil.ReadDir(imagePath)
	// if err != nil {
	// 	return FListInfo{}, err
	// }
	// out, err := exec.Command("mountpoint", imagePath).CombinedOutput()
	// if err != nil {
	// 	return FListInfo{}, err
	// }
	// log.Debug().Msgf("mountpoint: %s", string(out))
	// log.Debug().Str("mnt", imagePath).Msg("listing files in")
	// for _, ent := range entities {
	// 	log.Debug().Str("file", ent.Name()).Msg("file found")
	// }

	kernel := filepath.Join(imagePath, "kernel")
	log.Debug().Str("file", kernel).Msg("checking kernel")
	if _, err := os.Stat(kernel); os.IsNotExist(err) {
		return FListInfo{Container: true}, nil
	} else if err != nil {
		return FListInfo{}, errors.Wrap(err, "couldn't stat /kernel")
	}

	initrd := filepath.Join(imagePath, "initrd")
	log.Debug().Str("file", initrd).Msg("checking initrd")
	if _, err := os.Stat(initrd); os.IsNotExist(err) {
		initrd = "" // optional
	} else if err != nil {
		return FListInfo{}, errors.Wrap(err, "couldn't state /initrd")
	}

	image := imagePath + "/image.raw"
	log.Debug().Str("file", image).Msg("checking image")
	if _, err := os.Stat(image); err != nil {
		return FListInfo{}, errors.Wrap(err, "couldn't stat /image.raw")
	}

	return FListInfo{Initrd: initrd, Kernel: kernel, ImagePath: image}, nil
}

type startup struct {
	Entries map[string]entry `toml:"startup"`
}

type entry struct {
	Name string
	Args args
}

type args struct {
	Name string
	Dir  string
	Args []string
	Env  map[string]string
}

func (e entry) Entrypoint() string {
	if e.Name == "core.system" ||
		e.Name == "core.base" && e.Args.Name != "" {
		var buf strings.Builder

		buf.WriteString(e.Args.Name)
		for _, arg := range e.Args.Args {
			buf.WriteRune(' ')
			arg = strings.Replace(arg, "\"", "\\\"", -1)
			buf.WriteRune('"')
			buf.WriteString(arg)
			buf.WriteRune('"')
		}

		return buf.String()
	}

	return ""
}

func (e entry) WorkingDir() string {
	return e.Args.Dir
}

func (e entry) Envs() map[string]string {
	return e.Args.Env
}

// This code is backward compatible with flist .startup.toml file
// where the flist can define an Entrypoint and some initial environment
// variables. this is used *with* the container configuration like this
// - if no zmachine entry point is defined, use the one from .startup.toml
// - if envs are defined in flist, merge with the env variables from the
func fListStartup(data *zos.ZMachine, path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return errors.Wrapf(err, "failed to load startup file '%s'", path)
	}

	defer f.Close()

	log.Info().Msg("startup file found")
	startup := startup{}
	if _, err := toml.DecodeReader(f, &startup); err != nil {
		return err
	}

	entry, ok := startup.Entries["entry"]
	if !ok {
		return nil
	}

	data.Env = mergeEnvs(entry.Envs(), data.Env)

	if data.Entrypoint == "" && entry.Entrypoint() != "" {
		data.Entrypoint = entry.Entrypoint()
	}
	return nil
}

// mergeEnvs new into base
func mergeEnvs(base, new map[string]string) map[string]string {
	if len(base) == 0 {
		return new
	}

	for k, v := range new {
		base[k] = v
	}

	return base
}
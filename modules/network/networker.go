package network

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/threefoldtech/zosv2/modules/network/ndmz"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/pkg/errors"

	"github.com/threefoldtech/zosv2/modules/network/ifaceutil"

	"github.com/threefoldtech/zosv2/modules/network/macvlan"
	"github.com/threefoldtech/zosv2/modules/network/nr"
	"github.com/threefoldtech/zosv2/modules/network/types"
	"github.com/threefoldtech/zosv2/modules/versioned"

	"github.com/rs/zerolog/log"

	"github.com/threefoldtech/zosv2/modules/network/namespace"

	"github.com/threefoldtech/zosv2/modules"
)

const (
	// ZDBIface is the name of the interface used in the 0-db network namespace
	ZDBIface = "zdb0"
)

type networker struct {
	identity   modules.IdentityManager
	storageDir string
	tnodb      TNoDB
}

// NewNetworker create a new modules.Networker that can be used over zbus
func NewNetworker(identity modules.IdentityManager, tnodb TNoDB, storageDir string) modules.Networker {
	nw := &networker{
		identity:   identity,
		storageDir: storageDir,
		tnodb:      tnodb,
	}

	return nw
}

var _ modules.Networker = (*networker)(nil)

func validateNetwork(n *modules.Network) error {
	// TODO
	// if n.NetID == "" {
	// 	return fmt.Errorf("network ID cannot be empty")
	// }

	// if n.PrefixZero == nil {
	// 	return fmt.Errorf("PrefixZero cannot be empty")
	// }

	// if len(n.Resources) < 1 {
	// 	return fmt.Errorf("Network needs at least one network resource")
	// }

	// for i, r := range n.Resources {
	// 	nibble, err := nib.NewNibble(r.Prefix, n.AllocationNR)
	// 	if err != nil {
	// 		return errors.Wrap(err, "allocation prefix is not valid")
	// 	}
	// 	if r.Prefix == nil {
	// 		return fmt.Errorf("Prefix for network resource %s is empty", r.NodeID.Identity())
	// 	}

	// 	peer := r.Peers[i]
	// 	expectedPort := nibble.WireguardPort()
	// 	if peer.Connection.Port != 0 && peer.Connection.Port != expectedPort {
	// 		return fmt.Errorf("Wireguard port for peer %s should be %d", r.NodeID.Identity(), expectedPort)
	// 	}

	// 	if peer.Connection.IP != nil && !peer.Connection.IP.IsGlobalUnicast() {
	// 		return fmt.Errorf("Wireguard endpoint for peer %s should be a public IP, not %s", r.NodeID.Identity(), peer.Connection.IP.String())
	// 	}
	// }

	// if n.Exit == nil {
	// 	return fmt.Errorf("Exit point cannot be empty")
	// }

	// if n.AllocationNR < 0 {
	// 	return fmt.Errorf("AllocationNR cannot be negative")
	// }

	return nil
}

func (n *networker) Join(networkdID modules.NetID, containerID string, addrs []string) (join modules.Member, err error) {
	// TODO:
	// 1- Make sure this network id is actually deployed
	// 2- Create a new namespace, then create a veth pair inside this namespace
	// 3- Hook one end to the NR bridge
	// 4- Assign IP to the veth endpoint inside the namespace.
	// 5- return the namespace name

	log.Info().Str("network-id", string(networkdID)).Msg("joining network")

	network, err := n.networkOf(string(networkdID))
	if err != nil {
		return join, errors.Wrapf(err, "couldn't load network with id (%s)", networkdID)
	}

	nodeID := n.identity.NodeID().Identity()
	localNR, err := ResourceByNodeID(nodeID, network.NetResources)
	if err != nil {
		return join, err
	}

	netRes, err := nr.New(networkdID, localNR)
	if err != nil {
		return join, errors.Wrap(err, "failed to load network resource")
	}

	ips := make([]net.IP, len(addrs))
	for i, addr := range addrs {
		ips[i] = net.ParseIP(addr)
	}

	return netRes.Join(containerID, ips)
}

// ZDBPrepare sends a macvlan interface into the
// network namespace of a ZDB container
func (n networker) ZDBPrepare() (string, error) {

	netNSName, err := ifaceutil.RandomName("zdb-ns-")
	if err != nil {
		return "", err
	}

	netNs, err := createNetNS(netNSName)
	if err != nil {
		return "", err
	}
	defer netNs.Close()

	// find which interface to use as master for the macvlan
	pubIface := DefaultBridge
	if namespace.Exists(types.PublicNamespace) {
		master, err := publicMasterIface()
		if err != nil {
			return "", errors.Wrap(err, "failed to retrieve the master interface name of the public interface")
		}
		pubIface = master
	}

	macVlan, err := macvlan.Create(ZDBIface, pubIface, netNs)
	if err != nil {
		return "", errors.Wrap(err, "failed to create public mac vlan interface")
	}

	// we don't set any route or ip
	if err := macvlan.Install(macVlan, []*net.IPNet{}, []*netlink.Route{}, netNs); err != nil {
		return "", err
	}

	return netNSName, nil
}

// Addrs return the IP addresses of interface
func (n networker) Addrs(iface string, netns string) ([]net.IP, error) {
	var ips []net.IP

	f := func(_ ns.NetNS) error {
		link, err := netlink.LinkByName(iface)
		if err != nil {
			return errors.Wrapf(err, "failed to get interface %s", iface)
		}

		addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			return errors.Wrapf(err, "failed to list addresses of interfaces %s", iface)
		}
		ips = make([]net.IP, len(addrs))
		for i, addr := range addrs {
			ips[i] = addr.IP
		}
		return nil
	}

	if netns != "" {
		netNS, err := namespace.GetByName(netns)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get network namespace %s", netns)
		}
		defer netNS.Close()

		if err := netNS.Do(f); err != nil {
			return nil, err
		}
	} else {
		if err := f(nil); err != nil {
			return nil, err
		}
	}

	return ips, nil
}

// CreateNR implements modules.Networker interface
func (n *networker) CreateNR(network modules.Network) (string, error) {
	var err error

	// TODO: fix me
	// if err := validateNetwork(&network); err != nil {
	// 	log.Error().Err(err).Msg("network object format invalid")
	// 	return "", err
	// }

	b, err := json.Marshal(network)
	if err != nil {
		panic(err)
	}
	log.Debug().
		Str("network", string(b)).
		Msg("create NR")

	netNR, err := ResourceByNodeID(n.identity.NodeID().Identity(), network.NetResources)
	if err != nil {
		return "", err
	}

	privateKey, err := n.extractPrivateKey(netNR.WGPrivateKey)
	if err != nil {
		return "", errors.Wrap(err, "failed to extract private key from network object")
	}

	netr, err := nr.New(network.NetID, netNR)
	if err != nil {
		return "", err
	}

	cleanup := func() {
		log.Error().Msg("clean up network resource")
		if err := netr.Delete(); err != nil {
			log.Error().Err(err).Msg("error during deletion of network resource after failed deployment")
		}
	}

	// this is ok if pubNS is nil, nr.Create handles it
	pubNS, _ := namespace.GetByName(types.PublicNamespace)

	log.Info().Msg("create network resource namespace")
	if err := netr.Create(pubNS); err != nil {
		cleanup()
		return "", errors.Wrap(err, "failed to create network resource")
	}

	if err := ndmz.AttachNR(string(network.NetID), netr); err != nil {
		return "", errors.Wrapf(err, "failed to attach network resource to DMZ bridge")
	}

	if err := netr.ConfigureWG(privateKey); err != nil {
		cleanup()
		return "", errors.Wrap(err, "failed to configure network resource")
	}

	// map the network ID to the network namespace
	path := filepath.Join(n.storageDir, string(network.NetID))
	file, err := os.Create(path)
	if err != nil {
		cleanup()
		return "", err
	}
	defer file.Close()
	writer, err := versioned.NewWriter(file, modules.NetworkSchemaLatestVersion)
	if err != nil {
		cleanup()
		return "", err
	}

	enc := json.NewEncoder(writer)
	if err := enc.Encode(&network); err != nil {
		cleanup()
		return "", errors.Wrap(err, "failed to store network object")
	}

	return netr.Namespace()
}

func (n *networker) networkOf(id string) (*modules.Network, error) {
	path := filepath.Join(n.storageDir, string(id))
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader, err := versioned.NewReader(file)
	if versioned.IsNotVersioned(err) {
		// old data that doesn't have any version information
		if _, err := file.Seek(0, 0); err != nil {
			return nil, err
		}

		reader = versioned.NewVersionedReader(versioned.MustParse("0.0.0"), file)
	} else if err != nil {
		return nil, err
	}

	var net modules.Network
	dec := json.NewDecoder(reader)

	validV1 := versioned.MustParseRange(fmt.Sprintf("<=%s", modules.NetworkSchemaV1))

	if validV1(reader.Version()) {
		if err := dec.Decode(&net); err != nil {
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("unknown network object version (%s)", reader.Version())
	}

	return &net, nil
}

// DeleteNR implements modules.Networker interface
func (n *networker) DeleteNR(network modules.Network) error {
	netNR, err := ResourceByNodeID(n.identity.NodeID().Identity(), network.NetResources)
	if err != nil {
		return err
	}

	nr, err := nr.New(network.NetID, netNR)
	if err != nil {
		return errors.Wrap(err, "failed to load network resource")
	}

	if err := nr.Delete(); err != nil {
		return errors.Wrap(err, "failed to delete network resource")
	}

	// map the network ID to the network namespace
	path := filepath.Join(n.storageDir, string(network.NetID))
	if err := os.Remove(path); err != nil {
		log.Error().Err(err).Msg("failed to remove file mapping between network ID and namespace")
	}

	return nil
}

func (n *networker) extractPrivateKey(hexKey string) (wgtypes.Key, error) {
	//FIXME zaibon: I would like to move this into the nr package,
	// but this method requires the identity module which is only available
	// on the networker object

	key := wgtypes.Key{}

	sk, err := hex.DecodeString(hexKey)
	if err != nil {
		return key, err
	}
	decoded, err := n.identity.Decrypt([]byte(sk))
	if err != nil {
		return key, err
	}

	return wgtypes.ParseKey(string(decoded))
}

// publicMasterIface return the name of the master interface
// of the public interface
func publicMasterIface() (string, error) {
	netns, err := namespace.GetByName(types.PublicNamespace)
	if err != nil {
		return "", err
	}
	defer netns.Close()

	var iface string
	if err := netns.Do(func(_ ns.NetNS) error {
		pl, err := netlink.LinkByName(types.PublicIface)
		if err != nil {
			return err
		}
		index := pl.Attrs().MasterIndex
		if index == 0 {
			return fmt.Errorf("public iface has not master")
		}
		ml, err := netlink.LinkByIndex(index)
		if err != nil {
			return err
		}
		iface = ml.Attrs().Name
		return nil
	}); err != nil {
		return "", err
	}

	return iface, nil
}

// createNetNS create a network namespace and set lo interface up
func createNetNS(name string) (ns.NetNS, error) {

	netNs, err := namespace.Create(name)
	if err != nil {
		return nil, err
	}

	err = netNs.Do(func(_ ns.NetNS) error {
		return ifaceutil.SetLoUp()
	})
	if err != nil {
		namespace.Delete(netNs)
		return nil, err
	}

	return netNs, nil
}

// ResourceByNodeID return the net resource associated with a nodeID
func ResourceByNodeID(nodeID string, resources []*modules.NetResource) (*modules.NetResource, error) {
	for _, resource := range resources {
		if resource.NodeID == nodeID {
			return resource, nil
		}
	}
	return nil, fmt.Errorf("not network resource for this node: %s", nodeID)
}

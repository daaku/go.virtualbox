// Package virtualbox provides a library to interact with VirtualBox.
package virtualbox

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"github.com/nshah/go.homedir"
	uuid "github.com/nshah/gouuid"
	"log"
	"os"
	"os/exec"
	"path"
	"regexp"
	"runtime"
	"strconv"
)

type HardDiskFormat string

const (
	VDI = HardDiskFormat("VDI")
)

type HardDiskType string

const (
	Normal    = HardDiskType("")
	Immutable = HardDiskType("Immutable")
)

type Status string

const (
	Off     = Status("Off")
	Running = Status("Running")
)

type HardDisk struct {
	UUID      uuid.UUID
	Location  string
	Format    HardDiskFormat
	Type      HardDiskType
	AutoReset bool         `json:",omitempty"`
	Children  []*uuid.UUID `json:",omitempty"`
	Parent    *uuid.UUID   `json:",omitempty"`
}

type Machine struct {
	UUID         uuid.UUID
	Name         string
	Source       string
	OSType       OSType
	Status       Status `json:",omitempty"`
	HardDisks    []*uuid.UUID
	VRDEPort     int `json:",omitempty"`
	SeleniumPort int `json:",omitempty"`
}

type HardDiskMap map[uuid.UUID]*HardDisk
type MachineMap map[uuid.UUID]*Machine

type VirtualBox struct {
	HardDisks HardDiskMap
	Machines  MachineMap
}

type xmlMachineListEntry struct {
	UUID   string `xml:"uuid,attr"`
	Source string `xml:"src,attr"`
}

type xmlMachineList struct {
	XMLName  xml.Name              `xml:"VirtualBox"`
	Machines []xmlMachineListEntry `xml:"Global>MachineRegistry>MachineEntry"`
}

type xmlHardDisk struct {
	UUID      string         `xml:"uuid,attr"`
	Location  string         `xml:"location,attr"`
	Format    HardDiskFormat `xml:"format,attr"`
	Type      HardDiskType   `xml:"type,attr"`
	AutoReset bool           `xml:"autoReset,attr"`
	Children  []xmlHardDisk  `xml:"HardDisk"`
}

type xmlVrdeProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type xmlRemoteDisplay struct {
	Enabled    bool              `xml:"enabled,attr"`
	Properties []xmlVrdeProperty `xml:"VRDEProperties>Property"`
}

type xmlNetworkForwarding struct {
	Name      string `xml:"name,attr"`
	HostPort  int    `xml:"hostport,attr"`
	GuestPort int    `xml:"guestport,attr"`
}

type xmlAttachedDisk struct {
	UUID string `xml:"uuid,attr"`
}

type xmlMachine struct {
	Name                string                 `xml:"name,attr"`
	OSType              string                 `xml:"OSType,attr"`
	RegisteredHardDisks []xmlHardDisk          `xml:"MediaRegistry>HardDisks>HardDisk"`
	RemoteDisplay       xmlRemoteDisplay       `xml:"Hardware>RemoteDisplay"`
	Forwarding          []xmlNetworkForwarding `xml:"Hardware>Network>Adapter>NAT>Forwarding"`
	AttachedHardDisks   []xmlAttachedDisk      `xml:"StorageControllers>StorageController>AttachedDevice>Image"`
}

type xmlMachineRoot struct {
	XMLName  xml.Name     `xml:"VirtualBox"`
	Machines []xmlMachine `xml:"Machine"`
}

// Load the given configuration file
func Decode(configPath string) (vbox *VirtualBox, err error) {
	runningMachineUUIDs, err := runningMachineUUIDs()
	if err != nil {
		return
	}

	// top level xml file
	file, err := os.Open(configPath)
	if err != nil {
		return
	}
	machineList := new(xmlMachineList)
	err = xml.NewDecoder(file).Decode(machineList)
	if err != nil {
		return
	}

	// per machine xml file
	vbox = new(VirtualBox)
	vbox.Machines = make(MachineMap, len(machineList.Machines))
	vbox.HardDisks = make(HardDiskMap)

	for _, machineListEntry := range machineList.Machines {
		file, err := os.Open(machineListEntry.Source)
		if err != nil {
			return nil, err
		}

		xmlMachineRoot := new(xmlMachineRoot)
		err = xml.NewDecoder(file).Decode(xmlMachineRoot)
		if err != nil {
			return nil, err
		}

		if len(xmlMachineRoot.Machines) != 1 {
			return nil, errors.New("Was expecting exactly 1 machine.")
		}
		xmlMachine := xmlMachineRoot.Machines[0]

		machineUUID, err := uuid.ParseHex(machineListEntry.UUID)
		if err != nil {
			return nil, err
		}

		status := Off
		if runningMachineUUIDs[*machineUUID] {
			status = Running
		}

		vrdePort := 0
		if xmlMachine.RemoteDisplay.Enabled {
			vrdePortString := findProperty(&xmlMachine.RemoteDisplay.Properties,
				"TCP/Ports")
			if vrdePortString != "" {
				vrdePort, err = strconv.Atoi(vrdePortString)
				if err != nil {
					return nil, err
				}
			}
		}

		seleniumPort := 0
		for _, forwarding := range xmlMachine.Forwarding {
			if forwarding.Name == "selenium" {
				seleniumPort = forwarding.HostPort
			}
		}

		machine := &Machine{
			UUID:         *machineUUID,
			Source:       machineListEntry.Source,
			Name:         xmlMachine.Name,
			OSType:       OSType(xmlMachine.OSType),
			Status:       status,
			VRDEPort:     vrdePort,
			SeleniumPort: seleniumPort,
		}

		for _, xmlHardDisk := range xmlMachine.RegisteredHardDisks {
			_, err := vbox.HardDisks.AddHardDisks(
				&xmlHardDisk, nil, path.Dir(machine.Source))
			if err != nil {
				return nil, err
			}
		}

		machine.HardDisks = make([]*uuid.UUID, len(xmlMachine.AttachedHardDisks))
		for index, attachedImage := range xmlMachine.AttachedHardDisks {
			imageUUID, err := uuid.ParseHex(attachedImage.UUID)
			if err != nil {
				return nil, err
			}
			machine.HardDisks[index] = imageUUID
		}

		vbox.Machines[*machineUUID] = machine
	}

	return
}

func (hardDisks HardDiskMap) AddHardDisks(xmlHardDisk *xmlHardDisk, parent *uuid.UUID, dir string) (disk *HardDisk, err error) {
	diskUUID, err := uuid.ParseHex(xmlHardDisk.UUID)
	if err != nil {
		return nil, err
	}
	disk = &HardDisk{
		UUID:      *diskUUID,
		Location:  xmlHardDisk.Location,
		Format:    xmlHardDisk.Format,
		Type:      xmlHardDisk.Type,
		AutoReset: xmlHardDisk.AutoReset,
		Parent:    parent,
	}

	if !path.IsAbs(disk.Location) {
		disk.Location = path.Join(dir, disk.Location)
	}

	lenChildDisks := len(xmlHardDisk.Children)
	if lenChildDisks != 0 {
		disk.Children = make([]*uuid.UUID, lenChildDisks)
		for index, childXmlDisk := range xmlHardDisk.Children {
			childDisk, err := hardDisks.AddHardDisks(&childXmlDisk, &disk.UUID, dir)
			if err != nil {
				return nil, err
			}
			disk.Children[index] = &childDisk.UUID
		}
	}

	hardDisks[*diskUUID] = disk

	return disk, nil
}

func findProperty(properties *[]xmlVrdeProperty, name string) string {
	for _, property := range *properties {
		if property.Name == name {
			return property.Value
		}
	}
	return ""
}

func (disks HardDiskMap) MarshalJSON() ([]byte, error) {
	disksStrings := make(map[string]*HardDisk, len(disks))
	for uuid, disk := range disks {
		disksStrings[uuid.String()] = disk
	}
	return json.Marshal(disksStrings)
}

func (machines MachineMap) MarshalJSON() ([]byte, error) {
	machinesStrings := make(map[string]*Machine, len(machines))
	for uuid, machine := range machines {
		machinesStrings[uuid.String()] = machine
	}
	return json.Marshal(machinesStrings)
}

func (machine *Machine) PowerOff() error {
	err := exec.Command(
		"VBoxManage", "controlvm", machine.UUID.String(), "poweroff").Run()
	if err != nil {
		return err
	}
	machine.Status = Off
	return nil
}

func (machine *Machine) Start(headless bool) error {
	startType := "gui"
	if headless {
		startType = "headless"
	}
	err := exec.Command(
		"VBoxManage", "startvm", machine.UUID.String(), "--type", startType).Run()
	if err != nil {
		return err
	}
	machine.Status = Running
	return nil
}

func extractUUIDs(text string) (uuids []*uuid.UUID) {
	re, err := regexp.Compile("([[:xdigit:]]{8}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{12})")
	if err != nil {
		log.Fatal("Failed to compile UUID regexp.")
	}

	uuidStrings := re.FindAllString(text, -1)
	uuids = make([]*uuid.UUID, len(uuidStrings))
	for index, uuidString := range uuidStrings {
		uuid, err := uuid.ParseHex(uuidString)
		if err != nil {
			log.Fatal("Failed to compile parse UUID despite validating regexp.")
		}
		uuids[index] = uuid
	}
	return uuids
}

// Get a map of UUIDs for running machines
func runningMachineUUIDs() (uuids map[uuid.UUID]bool, err error) {
	bytes, err := exec.Command("VBoxManage", "list", "runningvms").Output()
	if err != nil {
		return nil, err
	}

	uuidsArray := extractUUIDs(string(bytes))
	uuids = make(map[uuid.UUID]bool, len(uuidsArray))
	for _, uuid := range uuidsArray {
		uuids[*uuid] = true
	}
	return
}

// Default file path for the VirtualBox config for the current user
func DefaultPath() string {
	switch runtime.GOOS {
	case "darwin":
		return path.Join(homedir.Get(), "Library/VirtualBox/VirtualBox.xml")
	}
	return path.Join(homedir.Get(), ".VirtualBox/VirtualBox.xml")
}

// Decode the default VirtualBox configuration for the current user
func DecodeDefault() (vbox *VirtualBox, err error) {
	return Decode(DefaultPath())
}

type CreateMachine struct {
	Name       string
	OSType     OSType
	Register   bool
	BaseFolder string
}

func (createMachine CreateMachine) Create() (*uuid.UUID, error) {
	register := ""
	if createMachine.Register {
		register = "--register"
	}

	bytes, err := exec.Command(
		"VBoxManage", "createvm",
		"--name", createMachine.Name,
		"--ostype", string(createMachine.OSType),
		register,
		"--basefolder", createMachine.BaseFolder).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("Error in createvm, err: %s", err)
	}
	uuids := extractUUIDs(string(bytes))
	if len(uuids) != 1 {
		log.Fatal("Was expecting exactly 1 UUID.")
	}

	return uuids[0], nil
}

func (disk *HardDisk) EnsureAutoReset() error {
	if !disk.AutoReset {
		err := exec.Command(
			"VBoxManage", "modifyhd",
			disk.UUID.String(),
			"--autoreset", "on").Run()
		if err != nil {
			return err
		}
		disk.AutoReset = true
	}
	return nil
}

func (machine *Machine) SeleniumAddress() string {
	return "0.0.0.0:" + strconv.Itoa(machine.SeleniumPort)
}

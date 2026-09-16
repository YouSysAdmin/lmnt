package guest

import (
	"io"
	"log/slog"
	"testing"
)

// lsblkSample was captured from Alpine 3.24's util-linux lsblk with an LVM
// image, an opened LUKS image and a plain ext4 image attached.
const lsblkSample = `{
   "blockdevices": [
      {
         "name": "loop1", "path": "/dev/loop1", "size": "96M", "fstype": "LVM2_member", "label": null, "type": "loop",
         "children": [
            {"name": "vgt-data", "path": "/dev/mapper/vgt-data", "size": "92M", "fstype": "ext4", "label": "lvmdata", "type": "lvm"}
         ]
      },{
         "name": "loop2", "path": "/dev/loop2", "size": "96M", "fstype": "crypto_LUKS", "label": null, "type": "loop",
         "children": [
            {"name": "cryptmnt", "path": "/dev/mapper/cryptmnt", "size": "80M", "fstype": "ext4", "label": "luksdata", "type": "crypt"}
         ]
      },{
         "name": "loop3", "path": "/dev/loop3", "size": "64M", "fstype": "ext4", "label": "lmnttest", "type": "loop"
      },{
         "name": "vda", "path": "/dev/vda", "size": "1G", "fstype": null, "label": null, "type": "disk",
         "children": [
            {"name": "vda1", "path": "/dev/vda1", "size": "160M", "fstype": "vfat", "label": null, "type": "part"},
            {"name": "vda3", "path": "/dev/vda3", "size": "606M", "fstype": "LVM2_member", "label": null, "type": "part",
             "children": [{"name": "vg0-root", "path": "/dev/mapper/vg0-root", "size": "500M", "fstype": "xfs", "label": null, "type": "lvm"}]}
         ]
      },{
         "name": "bad name", "path": "/dev/bad name", "size": "1M", "fstype": null, "label": null, "type": "disk"
      }
   ]
}`

func TestParseBlockDevices(t *testing.T) {
	devs, err := ParseBlockDevices([]byte(lsblkSample))
	if err != nil {
		t.Fatal(err)
	}

	want := []struct {
		device string
		depth  int
		fstype string
		typ    string
	}{
		{"loop1", 0, "LVM2_member", "loop"},
		{"mapper/vgt-data", 1, "ext4", "lvm"},
		{"loop2", 0, "crypto_LUKS", "loop"},
		{"mapper/cryptmnt", 1, "ext4", "crypt"},
		{"loop3", 0, "ext4", "loop"},
		{"vda", 0, "", "disk"},
		{"vda1", 1, "vfat", "part"},
		{"vda3", 1, "LVM2_member", "part"},
		{"mapper/vg0-root", 2, "xfs", "lvm"},
	}

	if len(devs) != len(want) {
		t.Fatalf("got %d devices, want %d: %+v", len(devs), len(want), devs)
	}

	for i, w := range want {
		d := devs[i]
		if d.Device() != w.device || d.Depth != w.depth || d.FSType != w.fstype || d.Type != w.typ {
			t.Errorf("#%d: got %s depth=%d fstype=%q type=%q, want %+v", i, d.Device(), d.Depth, d.FSType, d.Type, w)
		}
		if d.Children != nil {
			t.Errorf("#%d: children not cleared", i)
		}
	}

	if !devs[0].IsContainer() || !devs[2].IsContainer() || devs[4].IsContainer() {
		t.Error("IsContainer misclassifies LVM2_member/crypto_LUKS/ext4")
	}

	if devs[5].Label != "" {
		t.Errorf("null label decoded as %q", devs[5].Label)
	}
}

func TestParseBlockDevicesRejectsGarbage(t *testing.T) {
	if _, err := ParseBlockDevices([]byte("NAME SIZE\nvda 1G\n")); err == nil {
		t.Error("expected an error for non-JSON input")
	}

	devs, err := ParseBlockDevices([]byte(`{"blockdevices": []}`))
	if err != nil || len(devs) != 0 {
		t.Errorf("empty list: got %v, %v", devs, err)
	}
}

func TestBlockDevicesCommand(t *testing.T) {
	fake := newFake()
	fake.respond("lsblk", fakeResponse{stdout: `{"blockdevices":[{"name":"vdb","path":"/dev/vdb","size":"64M","fstype":"ext4","label":"x","type":"disk"}]}`})
	g := New(slog.New(slog.NewTextHandler(io.Discard, nil)), fake)

	devs, err := g.BlockDevices(t.Context(), "vdb")
	if err != nil {
		t.Fatal(err)
	}

	if len(devs) != 1 || devs[0].Device() != "vdb" {
		t.Errorf("got %+v", devs)
	}

	if got := fake.scripts[len(fake.scripts)-1]; got != "lsblk --json --output NAME,PATH,SIZE,FSTYPE,LABEL,TYPE /dev/vdb" {
		t.Errorf("script = %q", got)
	}

	if _, err := g.BlockDevices(t.Context(), "../etc"); err == nil {
		t.Error("bad device name accepted")
	}
}

func TestValidMountOptions(t *testing.T) {
	if err := ValidMountOptions("ro,subvol=@home,compress=zstd:3"); err != nil {
		t.Error(err)
	}
	if err := ValidMountOptions(""); err != nil {
		t.Error(err)
	}
	if err := ValidMountOptions("ro; rm -rf /"); err == nil {
		t.Error("shell metacharacters accepted")
	}
}

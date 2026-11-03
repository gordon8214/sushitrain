package sthome

import "t-shaped.nl/sushitrain/v2/winplatform"

func syncDir(path string) error { return winplatform.FlushDirectory(path) }

func privateImportedHome(path string) error { return winplatform.PrivateDirectory(path) }

// Copyright 2017-2023 the LinuxBoot Authors. All rights reserved
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:generate manifestcodegen

package bg

// SVN represents Security Version Number.
type SVN uint8

// SVN returns the Security Version Number of an SVN field
func (svn SVN) SVN() uint8 {
	return uint8(svn) & 0x0f
}

package app

import "relay/internal/buildinfo"

func (*Service) GetBuildInfo() buildinfo.Details { return buildinfo.Info() }

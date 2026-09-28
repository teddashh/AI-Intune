package probe

const lingerRoot = "/var/lib/systemd/linger"

func lingerFacts(unixUser string) (enabled, measured bool) {
	return lingerEnabledIn(lingerRoot, unixUser)
}

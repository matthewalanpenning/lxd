package acl

import (
	"fmt"
	"net"
	"strings"

	"github.com/pkg/errors"

	"github.com/lxc/lxd/lxd/network/openvswitch"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/validate"
)

// OVN ACL rule priorities.
const ovnACLPrioritySwitchAllow = 10
const ovnACLPriorityPortGroupAllow = 20
const ovnACLPriorityPortGroupReject = 30
const ovnACLPriorityPortGroupDrop = 40

// OVNACLPortGroupName returns the port group name for a Network ACL ID.
func OVNACLPortGroupName(networkACLID int64) openvswitch.OVNPortGroup {
	// OVN doesn't match port groups that have a "-" in them. So use an "_" for the separator.
	return openvswitch.OVNPortGroup(fmt.Sprintf("lxd_acl%d", networkACLID))
}

// OVNEnsureACLs ensures that the requested aclNames exist as OVN port groups (creates & applies ACL rules if not),
// and adds the requested addMembers to the new or existing OVN port groups. If reapplyRules is true then the
// current ACL rules in the database are applied to the existing port groups rather than just new ones.
// Any ACLs referenced in the requested ACLs rules are also created as empty OVN port groups if needed.
// If a requested ACL exists, but has no ACL rules applied, then the current rules are loaded out of the database
// and applied.
func OVNEnsureACLs(s *state.State, logger logger.Logger, client *openvswitch.OVN, aclProjectName string, aclNameIDs map[string]int64, aclNames []string, reapplyRules bool, addMembers ...openvswitch.OVNSwitchPort) (*revert.Reverter, error) {
	revert := revert.New()
	defer revert.Fail()

	// First check all ACL Names map to IDs in supplied aclNameIDs.
	for _, aclName := range aclNames {
		_, found := aclNameIDs[aclName]
		if !found {
			return nil, fmt.Errorf("Cannot find security ACL ID for %q", aclName)
		}
	}

	// Next check which OVN port groups need creating and which exist already.
	type aclStatus struct {
		name    string
		uuid    openvswitch.OVNPortGroupUUID
		aclInfo *api.NetworkACL
	}
	existingACLPortGroups := []aclStatus{}
	createACLPortGroups := []aclStatus{}

	for _, aclName := range aclNames {
		portGroupName := OVNACLPortGroupName(aclNameIDs[aclName])

		// Check if port group exists and has ACLs.
		portGroupUUID, hasACLs, err := client.PortGroupInfo(portGroupName)
		if err != nil {
			return nil, errors.Wrapf(err, "Failed getting port group UUID for security ACL %q setup", aclName)
		}

		if portGroupUUID == "" {
			// Load the config we'll need to create the port group with ACL rules.
			_, aclInfo, err := s.Cluster.GetNetworkACL(aclProjectName, aclName)
			if err != nil {
				return nil, errors.Wrapf(err, "Failed loading Network ACL %q", aclName)
			}

			createACLPortGroups = append(createACLPortGroups, aclStatus{name: aclName, aclInfo: aclInfo})
		} else {
			var aclInfo *api.NetworkACL

			// If we are being asked to forcefully reapply the rules, or if the port group exists but
			// doesn't have any rules, then we load the current rule set from the database to apply.
			// Note: An empty ACL list on a port group means it has only been partially setup, as
			// even LXD Network ACLs with no rules should have at least 1 OVN ACL applied because of
			// the default drop rule we add.
			if reapplyRules || !hasACLs {
				_, aclInfo, err = s.Cluster.GetNetworkACL(aclProjectName, aclName)
				if err != nil {
					return nil, errors.Wrapf(err, "Failed loading Network ACL %q", aclName)
				}

			}

			existingACLPortGroups = append(existingACLPortGroups, aclStatus{name: aclName, uuid: portGroupUUID, aclInfo: aclInfo})
		}
	}

	// Build a list of referenced ACLs in the rules of ACLs we need to create.
	// We will create port groups (without ACL rules) for any missing referenced ACL OVN port groups so that
	// when we add the rules for the new ACL port groups this doesn't trigger an OVN log error about missing
	// port groups.
	referencedACLs := make(map[string]struct{}, 0)
	for _, aclStatus := range createACLPortGroups {
		ovnAddReferencedACLs(s, aclProjectName, aclStatus.aclInfo, referencedACLs)
	}

	// Remove any references for our creation ACLs as we don't want to try and create them twice.
	for _, aclStatus := range createACLPortGroups {
		delete(referencedACLs, aclStatus.name)
	}

	// Retrieve instance port UUIDs to add to existing port groups if needed.
	var memberUUIDs map[openvswitch.OVNSwitchPort]openvswitch.OVNSwitchPortUUID
	if len(existingACLPortGroups) > 0 {
		memberUUIDs = make(map[openvswitch.OVNSwitchPort]openvswitch.OVNSwitchPortUUID, len(addMembers))

		for _, memberName := range addMembers {
			// Get logical port UUID.
			portUUID, err := client.LogicalSwitchPortUUID(memberName)
			if err != nil || portUUID == "" {
				return nil, errors.Wrapf(err, "Failed getting logical port UUID for %q for security ACL setup", memberName)
			}

			memberUUIDs[memberName] = portUUID
		}
	}

	// Create any missing port groups for the referenced ACLs before creating the requested ACL port groups.
	// This way the referenced port groups will exist for any rules that referenced them in the creation ACLs.
	// Note: We only create the empty port group, we do not add the ACL rules, so it is expected that any
	// future direct assignment of these referenced ACLs will trigger the ACL rules being added if needed.
	for aclName := range referencedACLs {
		portGroupName := OVNACLPortGroupName(aclNameIDs[aclName])

		// Check if port group exists.
		portGroupUUID, _, err := client.PortGroupInfo(portGroupName)
		if err != nil {
			return nil, errors.Wrapf(err, "Failed getting port group UUID for security ACL %q setup", aclName)
		}

		if portGroupUUID == "" {
			logger.Debug("Creating empty ACL OVN port group", log.Ctx{"networkACL": aclName, "portGroup": portGroupName})

			err := client.PortGroupAdd(portGroupName, addMembers...)
			if err != nil {
				return nil, errors.Wrapf(err, "Failed creating port group %q for referenced security ACL %q setup", portGroupName, aclName)
			}
			revert.Add(func() { client.PortGroupDelete(portGroupName) })
		}
	}

	// Create the needed port groups (and add requested members), and then apply ACL rules to new port group.
	for _, aclStatus := range createACLPortGroups {
		portGroupName := OVNACLPortGroupName(aclNameIDs[aclStatus.name])
		logger.Debug("Creating ACL OVN port group", log.Ctx{"networkACL": aclStatus.name, "portGroup": portGroupName, "addMembers": addMembers})

		err := client.PortGroupAdd(portGroupName, addMembers...)
		if err != nil {
			return nil, errors.Wrapf(err, "Failed creating port group %q for security ACL %q setup", portGroupName, aclStatus.name)
		}
		revert.Add(func() { client.PortGroupDelete(portGroupName) })

		_, aclInfo, err := s.Cluster.GetNetworkACL(aclProjectName, aclStatus.name)
		if err != nil {
			return nil, errors.Wrapf(err, "Failed loading Network ACL %q", aclStatus.name)
		}

		// Now apply our ACL rules to port group.
		err = ovnApplyToPortGroup(s, client, aclInfo, portGroupName, aclNameIDs)
		if err != nil {
			return nil, errors.Wrapf(err, "Failed applying ACL rules to port group %q for security ACL %q setup", portGroupName, aclStatus.name)
		}
	}

	// Add member ports to existing port groups.
	for _, aclStatus := range existingACLPortGroups {
		portGroupName := OVNACLPortGroupName(aclNameIDs[aclStatus.name])

		// If aclInfo has been loaded, then we should use it to apply ACL rules to the existing port group.
		if aclStatus.aclInfo != nil {
			logger.Debug("Applying ACL rules to OVN port group", log.Ctx{"networkACL": aclStatus.name, "portGroup": portGroupName})

			_, aclInfo, err := s.Cluster.GetNetworkACL(aclProjectName, aclStatus.name)
			if err != nil {
				return nil, errors.Wrapf(err, "Failed loading Network ACL %q", aclStatus.name)
			}

			err = ovnApplyToPortGroup(s, client, aclInfo, portGroupName, aclNameIDs)
			if err != nil {
				return nil, errors.Wrapf(err, "Failed applying ACL rules to port group %q for security ACL %q setup", portGroupName, aclStatus.name)
			}
		}

		if len(addMembers) > 0 {
			logger.Debug("Adding ACL OVN port group members", log.Ctx{"networkACL": aclStatus.name, "portGroup": portGroupName, "addMembers": addMembers})

			for _, memberName := range addMembers {
				err := client.PortGroupMemberAdd(portGroupName, memberUUIDs[memberName])
				if err != nil {
					return nil, errors.Wrapf(err, "Failed adding logical port %q to port group %q for security ACL %q setup", memberName, portGroupName, aclStatus.name)

				}
			}
		}
	}

	r := revert.Clone()
	revert.Success()
	return r, nil
}

// ovnAddReferencedACLs adds to the referencedACLNames any ACLs referenced by the rules in the supplied ACL.
func ovnAddReferencedACLs(s *state.State, aclProjectName string, info *api.NetworkACL, referencedACLNames map[string]struct{}) {
	addACLNamesFrom := func(ruleSubjects []string) {
		for _, subject := range ruleSubjects {
			if _, found := referencedACLNames[subject]; found {
				continue // Skip subjects already seen.
			}

			if validate.IsNetworkAddressCIDR(subject) == nil || validate.IsNetworkRange(subject) == nil {
				continue // Skip  if the subject is an IP CIDR or IP range.
			}

			// Anything else must be a referenced ACL name.
			// Record newly seen referenced ACL into authoriative list.
			referencedACLNames[subject] = struct{}{}
		}
	}

	for _, rule := range info.Ingress {
		addACLNamesFrom(util.SplitNTrimSpace(rule.Source, ",", -1, true))
	}

	for _, rule := range info.Egress {
		addACLNamesFrom(util.SplitNTrimSpace(rule.Destination, ",", -1, true))
	}
}

// ovnApplyToPortGroup applies the rules in the specified ACL to the specified port group.
func ovnApplyToPortGroup(s *state.State, client *openvswitch.OVN, aclInfo *api.NetworkACL, portGroupName openvswitch.OVNPortGroup, aclNameIDs map[string]int64) error {
	// Create slice for port group rules that has the capacity for ingress and egress rules, plus default drop.
	portGroupRules := make([]openvswitch.OVNACLRule, 0, len(aclInfo.Ingress)+len(aclInfo.Egress)+1)

	// convertACLRules converts the ACL rules to OVN ACL rules.
	convertACLRules := func(direction string, rules ...api.NetworkACLRule) error {
		for ruleIndex, rule := range rules {
			if rule.State == "disabled" {
				continue
			}

			portGroupRule, err := ovnRuleCriteriaToOVNPortGroupRule(direction, &rule, portGroupName, aclNameIDs)
			if err != nil {
				return err
			}

			if rule.State == "logged" {
				portGroupRule.Log = true
				portGroupRule.LogName = fmt.Sprintf("%s-%s-%d", portGroupName, direction, ruleIndex)
			}

			portGroupRules = append(portGroupRules, portGroupRule)
		}

		return nil
	}

	err := convertACLRules("ingress", aclInfo.Ingress...)
	if err != nil {
		return errors.Wrapf(err, "Failed converting ACL %q ingress rules for port group %q", aclInfo.Name, portGroupName)
	}

	err = convertACLRules("egress", aclInfo.Egress...)
	if err != nil {
		return errors.Wrapf(err, "Failed converting ACL %q egress rules for port group %q", aclInfo.Name, portGroupName)
	}

	// Add default drop rule to port group ACL.
	portGroupRules = append(portGroupRules, openvswitch.OVNACLRule{
		Direction: "to-lport", // Always use this so that outport is available to Match.
		Action:    "drop",
		Priority:  0, // Lowest priority to catch only unmatched traffic.
		Match:     fmt.Sprintf("inport == @%s || outport == @%s", portGroupName, portGroupName),
		Log:       true,
		LogName:   string(portGroupName),
	})

	err = client.PortGroupSetACLRules(portGroupName, portGroupRules...)
	if err != nil {
		return errors.Wrapf(err, "Failed applying ACL %q rules to port group %q", aclInfo.Name, portGroupName)
	}

	return nil
}

// ovnRuleCriteriaToOVNPortGroupRule converts an ACL rule into an OVNACLRule for a port group.
func ovnRuleCriteriaToOVNPortGroupRule(direction string, rule *api.NetworkACLRule, portGroupName openvswitch.OVNPortGroup, aclNameIDs map[string]int64) (openvswitch.OVNACLRule, error) {
	portGroupRule := openvswitch.OVNACLRule{
		Direction: "to-lport", // Always use this so that outport is available to Match.
	}

	// Populate Action and Priority based on rule's Action.
	switch rule.Action {
	case "allow":
		portGroupRule.Action = "allow-related" // TODO add stateless support.
		portGroupRule.Priority = ovnACLPriorityPortGroupAllow
	case "reject":
		portGroupRule.Action = "reject"
		portGroupRule.Priority = ovnACLPriorityPortGroupReject
	case "drop":
		portGroupRule.Action = "drop"
		portGroupRule.Priority = ovnACLPriorityPortGroupDrop
	}

	var matchParts []string

	// Add directional port filter so we only apply this rule to the ports in the port group.
	switch direction {
	case "ingress":
		matchParts = []string{fmt.Sprintf("outport == @%s", portGroupName)} // Traffic going to Instance.
	case "egress":
		matchParts = []string{fmt.Sprintf("inport == @%s", portGroupName)} // Traffic leaving Instance.
	default:
		matchParts = []string{fmt.Sprintf("inport == @%s || outport == @%s", portGroupName, portGroupName)}
	}

	// Add subject filters.
	if rule.Source != "" {
		match, err := ovnRuleSubjectToOVNACLMatch("src", aclNameIDs, util.SplitNTrimSpace(rule.Source, ",", -1, false)...)
		if err != nil {
			return openvswitch.OVNACLRule{}, err
		}

		matchParts = append(matchParts, match)
	}

	if rule.Destination != "" {
		match, err := ovnRuleSubjectToOVNACLMatch("dst", aclNameIDs, util.SplitNTrimSpace(rule.Destination, ",", -1, false)...)
		if err != nil {
			return openvswitch.OVNACLRule{}, err
		}

		matchParts = append(matchParts, match)
	}

	// Add protocol filters.
	if shared.StringInSlice(rule.Protocol, []string{"tcp", "udp"}) {
		matchParts = append(matchParts, fmt.Sprintf("%s", rule.Protocol))

		if rule.SourcePort != "" {
			matchParts = append(matchParts, ovnRulePortToOVNACLMatch(rule.Protocol, "src", util.SplitNTrimSpace(rule.SourcePort, ",", -1, false)...))
		}

		if rule.DestinationPort != "" {
			matchParts = append(matchParts, ovnRulePortToOVNACLMatch(rule.Protocol, "dst", util.SplitNTrimSpace(rule.DestinationPort, ",", -1, false)...))
		}
	} else if shared.StringInSlice(rule.Protocol, []string{"icmp4", "icmp6"}) {
		matchParts = append(matchParts, fmt.Sprintf("%s", rule.Protocol))

		if rule.ICMPType != "" {
			matchParts = append(matchParts, fmt.Sprintf("%s.type == %s", rule.Protocol, rule.ICMPType))
		}

		if rule.ICMPCode != "" {
			matchParts = append(matchParts, fmt.Sprintf("%s.code == %s", rule.Protocol, rule.ICMPCode))
		}
	}

	// Populate the Match field with the generated match parts.
	portGroupRule.Match = fmt.Sprintf("(%s)", strings.Join(matchParts, ") && ("))

	return portGroupRule, nil
}

// ovnRulePortToOVNACLMatch converts protocol (tcp/udp), direction (src/dst) and port criteria list into an OVN
// match statement.
func ovnRulePortToOVNACLMatch(protocol string, direction string, portCriteria ...string) string {
	fieldParts := make([]string, 0, len(portCriteria))

	for _, portCriterion := range portCriteria {
		criterionParts := strings.SplitN(portCriterion, "-", 2)
		if len(criterionParts) > 1 {
			fieldParts = append(fieldParts, fmt.Sprintf("(%s.%s >= %s && %s.%s <= %s)", protocol, direction, criterionParts[0], protocol, direction, criterionParts[1]))
		} else {
			fieldParts = append(fieldParts, fmt.Sprintf("%s.%s == %s", protocol, direction, criterionParts[0]))
		}
	}

	return strings.Join(fieldParts, " || ")
}

// ovnRuleSubjectToOVNACLMatch converts direction (src/dst) and subject criteria list into an OVN match statement.
func ovnRuleSubjectToOVNACLMatch(direction string, aclNameIDs map[string]int64, subjectCriteria ...string) (string, error) {
	fieldParts := make([]string, 0, len(subjectCriteria))

	// For each criterion check if value looks like an IP range or IP CIDR, and if not use it as an ACL name.
	for _, subjectCriterion := range subjectCriteria {
		if validate.IsNetworkRange(subjectCriterion) == nil {
			criterionParts := strings.SplitN(subjectCriterion, "-", 2)
			if len(criterionParts) > 1 {
				ip := net.ParseIP(criterionParts[0])
				if ip != nil {
					protocol := "ip4"
					if ip.To4() == nil {
						protocol = "ip6"
					}

					fieldParts = append(fieldParts, fmt.Sprintf("(%s.%s >= %s && %s.%s <= %s)", protocol, direction, criterionParts[0], protocol, direction, criterionParts[1]))
				}
			} else {
				return "", fmt.Errorf("Invalid IP range %q", subjectCriterion)
			}
		} else {
			ip, _, err := net.ParseCIDR(subjectCriterion)
			if err == nil {
				protocol := "ip4"
				if ip.To4() == nil {
					protocol = "ip6"
				}

				fieldParts = append(fieldParts, fmt.Sprintf("%s.%s == %s", protocol, direction, subjectCriterion))
			} else {
				// If not valid IP subnet, then assume this is an OVN port group name.
				portType := "inport"
				if direction == "dst" {
					portType = "outport"
				}

				aclID, found := aclNameIDs[subjectCriterion]
				if !found {
					return "", fmt.Errorf("Cannot find security ACL ID for %q", subjectCriterion)
				}

				fieldParts = append(fieldParts, fmt.Sprintf("%s == @%s", portType, OVNACLPortGroupName(aclID)))
			}
		}
	}

	return strings.Join(fieldParts, " || "), nil
}

// OVNApplyNetworkBaselineRules applies preset baseline logical switch rules to a allow access to network services.
func OVNApplyNetworkBaselineRules(client *openvswitch.OVN, switchName openvswitch.OVNSwitch, routerPortName openvswitch.OVNSwitchPort, intRouterIPs []*net.IPNet, dnsIPs []net.IP) error {
	rules := []openvswitch.OVNACLRule{
		{
			Direction: "to-lport",
			Action:    "allow",
			Priority:  ovnACLPrioritySwitchAllow,
			Match:     "arp || nd", // Neighbour discovery.
		},
		{
			Direction: "to-lport",
			Action:    "allow",
			Priority:  ovnACLPrioritySwitchAllow,
			Match:     fmt.Sprintf(`inport == "%s" && nd_ra`, routerPortName), // IPv6 router adverts from router.
		},
		{
			Direction: "to-lport",
			Action:    "allow",
			Priority:  ovnACLPrioritySwitchAllow,
			Match:     fmt.Sprintf(`outport == "%s" && nd_rs`, routerPortName), // IPv6 router solicitation to router.
		},
		{
			Direction: "to-lport",
			Action:    "allow",
			Priority:  ovnACLPrioritySwitchAllow,
			Match:     fmt.Sprintf(`outport == "%s" && ((ip4 && udp.dst == 67) || (ip6 && udp.dst == 547)) `, routerPortName), // DHCP to router.
		},
	}

	// Add rules to allow ping to/from internal router IPs.
	for _, intRouterIP := range intRouterIPs {
		ipVersion := 4
		icmpPingType := 8
		icmpPingReplyType := 0
		if intRouterIP.IP.To4() == nil {
			ipVersion = 6
			icmpPingType = 128
			icmpPingReplyType = 129
		}

		rules = append(rules,
			openvswitch.OVNACLRule{
				Direction: "to-lport",
				Action:    "allow",
				Priority:  ovnACLPrioritySwitchAllow,
				Match:     fmt.Sprintf(`outport == "%s" && icmp%d.type == %d && ip%d.dst == %s`, routerPortName, ipVersion, icmpPingType, ipVersion, intRouterIP.IP),
			},
			openvswitch.OVNACLRule{
				Direction: "to-lport",
				Action:    "allow",
				Priority:  ovnACLPrioritySwitchAllow,
				Match:     fmt.Sprintf(`inport == "%s" && icmp%d.type == %d && ip%d.src == %s`, routerPortName, ipVersion, icmpPingReplyType, ipVersion, intRouterIP.IP),
			},
		)
	}

	// Add rules to allow DNS to DNS IPs.
	for _, dnsIP := range dnsIPs {
		ipVersion := 4
		if dnsIP.To4() == nil {
			ipVersion = 6
		}

		rules = append(rules,
			openvswitch.OVNACLRule{
				Direction: "to-lport",
				Action:    "allow",
				Priority:  ovnACLPrioritySwitchAllow,
				Match:     fmt.Sprintf(`outport == "%s" && ip%d.dst == %s && (udp.dst == 53 || tcp.dst == 53)`, routerPortName, ipVersion, dnsIP),
			},
		)
	}

	err := client.LogicalSwitchSetACLRules(switchName, rules...)
	if err != nil {
		return errors.Wrapf(err, "Failed applying baseline ACL rules to logical switch %q", switchName)
	}

	return nil
}

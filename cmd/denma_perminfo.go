package main

// denmaPermInfo is how a permission is named and explained in the admin.
type denmaPermInfo struct {
	Key   string
	Label string
	What  string
	Risky bool
}

// denmaPermHidden are permissions no form offers in multi-center mode.
var denmaPermHidden = map[string]bool{"webhooks:post_bounce": true}

var denmaPermGroupLabels = map[string]string{
	"lists":       "Lists",
	"subscribers": "Contacts",
	"campaigns":   "Campaigns and automations",
	"bounces":     "Bounces",
	"media":       "Media",
	"templates":   "Templates",
	"users":       "People and roles",
	"settings":    "Config and hub settings",
}

var denmaPermInfos = map[string]denmaPermInfo{
	"lists:get_all": {
		Label: "See all lists",
		What:  "See every list, whatever their list role says.",
	},
	"lists:manage_all": {
		Label: "Manage all lists",
		What:  "Create lists, and edit or delete any list.",
	},
	"list:get": {
		Label: "See their lists",
		What:  "See this list, and its contacts and campaigns if their role allows.",
	},
	"list:manage": {
		Label: "Manage their lists",
		What:  "Edit or delete this list, and change its contacts and campaigns if their role allows.",
	},
	"subscribers:get": {
		Label: "See contacts on their lists",
		What:  "See and export contacts on lists they can see.",
	},
	"subscribers:get_all": {
		Label: "See all contacts",
		What:  "See and export every contact. Also opens Tags and the dashboard's figures.",
	},
	"subscribers:manage": {
		Label: "Change contacts",
		What:  "Add, edit, blocklist, delete and move contacts on lists they manage, and send confirmations. With all lists, also tags.",
	},
	"subscribers:import": {
		Label: "Import contacts",
		What:  "Upload a file of contacts. Adding its new tags also needs Change contacts.",
	},
	"tx:send": {
		Label: "Send single e-mails",
		What:  "Send one e-mail from a template. For Claude, through an MCP key: the admin has no page for it.",
	},
	"campaigns:get": {
		Label: "See campaigns on their lists",
		What:  "See and preview campaigns going to lists they can see.",
	},
	"campaigns:get_all": {
		Label: "See all campaigns",
		What:  "See and preview every campaign.",
	},
	"campaigns:get_analytics": {
		Label: "See analytics",
		What:  "Open Analytics. Its contacts and automations pages need those permissions too.",
	},
	"campaigns:manage": {
		Label: "Edit campaigns on their lists",
		What:  "Create, edit, test and delete campaigns going to lists they manage.",
	},
	"campaigns:manage_all": {
		Label: "Edit all campaigns",
		What:  "Create, edit, test and delete any campaign.",
	},
	"campaigns:send": {
		Label: "Start and stop campaigns",
		What:  "Start, schedule, pause or cancel campaigns they can edit. This e-mails people.",
	},
	"automations:get": {
		Label: "See automations",
		What:  "See automations, their schedule and reports.",
	},
	"automations:manage": {
		Label: "Change automations",
		What:  "Create, edit, test, switch on or off and delete automations. One that's on e-mails people.",
	},
	"bounces:get": {
		Label: "See bounces",
		What:  "See which addresses bounced or complained, and why.",
	},
	"bounces:manage": {
		Label: "Manage bounces",
		What:  "Delete bounce records and blocklist contacts who bounced.",
	},
	"media:get": {
		Label: "See media",
		What:  "See and choose uploaded files.",
	},
	"media:manage": {
		Label: "Change media",
		What:  "Upload, move and delete files, and make folders.",
	},
	"templates:get": {
		Label: "See templates",
		What:  "See and preview templates.",
	},
	"templates:manage": {
		Label: "Change templates",
		What:  "Create, edit and delete templates, set the default, and edit the center's design.",
	},
	"users:get": {
		Label: "See people",
		What:  "See who has access, with their addresses and roles.",
	},
	"users:manage": {
		Label: "Change people",
		What:  "Add, edit and remove people, but not give anyone more than they have.",
		Risky: true,
	},
	"roles:get": {
		Label: "See roles",
		What:  "See roles and list roles, and what each allows.",
	},
	"roles:manage": {
		Label: "Change roles",
		What:  "Create, edit and delete roles, but not give one more than they have.",
		Risky: true,
	},
	"settings:get": {
		Label: "See hub settings",
		What:  "Hub only: see its settings and logs.",
	},
	"settings:manage": {
		Label: "Change hub settings",
		What:  "Hub only: change settings for every center, like sending limits and mail servers.",
		Risky: true,
	},
	"settings:maintain": {
		Label: "Hub maintenance",
		What:  "Hub only: delete contacts, pending subscriptions and analytics in bulk.",
		Risky: true,
	},
	"center:manage": {
		Label: "Manage Config",
		What:  "Open Config and the Activity log: the center's details, webhooks, and API and MCP keys.",
		Risky: true,
	},
}

// denmaPerm is the permission's name and explanation.
func denmaPerm(key string) denmaPermInfo {
	info, ok := denmaPermInfos[key]
	if !ok {
		info.Label = key
	}
	info.Key = key
	return info
}

// denmaPermLabels is each permission's name, for the pages that show them in JS.
func denmaPermLabels() map[string]string {
	out := make(map[string]string, len(denmaPermInfos))
	for k, v := range denmaPermInfos {
		out[k] = v.Label
	}
	return out
}

// denmaPermShown leaves out the permissions no form offers.
func denmaPermShown(groups []permGroup) []permGroup {
	out := make([]permGroup, 0, len(groups))
	for _, g := range groups {
		ps := make([]string, 0, len(g.Permissions))
		for _, p := range g.Permissions {
			if !denmaPermHidden[p] {
				ps = append(ps, p)
			}
		}
		out = append(out, permGroup{Group: g.Group, Permissions: ps})
	}
	return out
}

// DenmaPerm gives the role form a permission's name and explanation.
func (v roleFormView) DenmaPerm(key string) denmaPermInfo {
	return denmaPerm(key)
}

// DenmaPermGroup gives the role form a permission group's name.
func (v roleFormView) DenmaPermGroup(group string) string {
	if l, ok := denmaPermGroupLabels[group]; ok {
		return l
	}
	return group
}

// DenmaListPerms gives the list role form the two list permissions.
func (v roleFormView) DenmaListPerms() []denmaPermInfo {
	return []denmaPermInfo{denmaPerm("list:get"), denmaPerm("list:manage")}
}

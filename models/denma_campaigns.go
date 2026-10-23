package models

// DenmaCampaignPeople is how many people opened and clicked a campaign
// (cmd/denma_campaign_people.go). Not Tracked: sent without individual
// tracking, so people aren't known.
type DenmaCampaignPeople struct {
	Opened  int
	Clicked int
	Tracked bool
}

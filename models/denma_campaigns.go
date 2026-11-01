package models

// DenmaCampaignPeople is how many people opened and clicked a campaign (cmd/denma_campaign_people.go).
type DenmaCampaignPeople struct {
	Opened  int
	Clicked int
	Tracked bool
}

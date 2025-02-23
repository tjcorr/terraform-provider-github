package github

import (
	"context"
	"log"
	"reflect"

	"github.com/google/go-github/v51/github"
	"github.com/hashicorp/terraform-plugin-sdk/helper/schema"
)

type MemberChange struct {
	Old, New map[string]interface{}
}

func resourceGithubTeamMembers() *schema.Resource {

	return &schema.Resource{
		Create: resourceGithubTeamMembersCreate,
		Read:   resourceGithubTeamMembersRead,
		Update: resourceGithubTeamMembersUpdate,
		Delete: resourceGithubTeamMembersDelete,
		Importer: &schema.ResourceImporter{
			State: schema.ImportStatePassthrough,
		},

		Schema: map[string]*schema.Schema{
			"team_id": {
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
				Description: "The GitHub team id or slug",
			},
			"members": {
				Type:        schema.TypeSet,
				Required:    true,
				Description: "List of team members.",
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"username": {
							Type:             schema.TypeString,
							Required:         true,
							DiffSuppressFunc: caseInsensitive(),
							Description:      "The user to add to the team.",
						},
						"role": {
							Type:         schema.TypeString,
							Optional:     true,
							Default:      "member",
							Description:  "The role of the user within the team. Must be one of 'member' or 'maintainer'.",
							ValidateFunc: validateValueFunc([]string{"member", "maintainer"}),
						},
					},
				},
			},
			"etag": {
				Type:     schema.TypeString,
				Computed: true,
			},
		},
	}
}

func resourceGithubTeamMembersCreate(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*Owner).v3client
	orgId := meta.(*Owner).id

	teamIdString := d.Get("team_id").(string)
	teamId, err := getTeamID(teamIdString, meta)
	if err != nil {
		return err
	}
	ctx := context.Background()

	members := d.Get("members").(*schema.Set)
	for _, mMap := range members.List() {
		memb := mMap.(map[string]interface{})
		username := memb["username"].(string)
		role := memb["role"].(string)

		log.Printf("[DEBUG] Creating team membership: %s/%s (%s)", teamIdString, username, role)
		_, _, err = client.Teams.AddTeamMembershipByID(ctx,
			orgId,
			teamId,
			username,
			&github.TeamAddTeamMembershipOptions{
				Role: role,
			},
		)
		if err != nil {
			return err
		}
	}

	d.SetId(teamIdString)

	return resourceGithubTeamMembersRead(d, meta)
}

func resourceGithubTeamMembersUpdate(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*Owner).v3client
	orgId := meta.(*Owner).id

	teamIdString := d.Get("team_id").(string)
	teamId, err := getTeamID(teamIdString, meta)
	if err != nil {
		return err
	}
	ctx := context.Background()

	o, n := d.GetChange("members")
	vals := make(map[string]*MemberChange)
	for _, raw := range o.(*schema.Set).List() {
		obj := raw.(map[string]interface{})
		k := obj["username"].(string)
		vals[k] = &MemberChange{Old: obj}
	}
	for _, raw := range n.(*schema.Set).List() {
		obj := raw.(map[string]interface{})
		k := obj["username"].(string)
		if _, ok := vals[k]; !ok {
			vals[k] = &MemberChange{}
		}
		vals[k].New = obj
	}

	for username, change := range vals {
		var create, delete bool

		switch {
		// create a new one if old is nil
		case change.Old == nil:
			create = true
		// delete existing if new is nil
		case change.New == nil:
			delete = true
			// no change
		case reflect.DeepEqual(change.Old, change.New):
			continue
			// recreate - role changed
		default:
			delete = true
			create = true
		}

		if delete {
			log.Printf("[DEBUG] Deleting team membership: %s/%s", teamIdString, username)

			_, err = client.Teams.RemoveTeamMembershipByID(ctx, orgId, teamId, username)
			if err != nil {
				return err
			}
		}

		if create {
			role := change.New["role"].(string)

			log.Printf("[DEBUG] Creating team membership: %s/%s (%s)", teamIdString, username, role)
			_, _, err = client.Teams.AddTeamMembershipByID(ctx,
				orgId,
				teamId,
				username,
				&github.TeamAddTeamMembershipOptions{
					Role: role,
				},
			)
			if err != nil {
				return err
			}
		}
	}

	d.SetId(teamIdString)

	return resourceGithubTeamMembersRead(d, meta)
}

func resourceGithubTeamMembersRead(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*Owner).v3client
	orgId := meta.(*Owner).id
	teamIdString := d.Get("team_id").(string)
	if teamIdString == "" && !d.IsNewResource() {
		log.Printf("[DEBUG] Importing team with id %q", d.Id())
		teamIdString = d.Id()
	}

	teamId, err := getTeamID(teamIdString, meta)
	if err != nil {
		return err
	}

	// We intentionally set these early to allow reconciliation
	// from an upstream bug which emptied team_id in state
	// See https://github.com/integrations/terraform-provider-github/issues/323
	d.Set("team_id", teamIdString)

	ctx := context.WithValue(context.Background(), ctxId, d.Id())
	if !d.IsNewResource() {
		ctx = context.WithValue(ctx, ctxEtag, d.Get("etag").(string))
	}

	etags := make([]string, 0)
	// List members & maintainers as list 'all' drops role information
	log.Printf("[DEBUG] Reading team members: %s", teamIdString)
	memberOptions := github.TeamListTeamMembersOptions{
		ListOptions: github.ListOptions{
			PerPage: maxPerPage,
		},
		Role: "member",
	}

	var members []*github.User
	for {
		member, resp, err := client.Teams.ListTeamMembersByID(ctx, orgId, teamId, &memberOptions)
		if err != nil {
			return err
		}

		etags = append(etags, resp.Header.Get("ETag"))
		members = append(members, member...)
		if resp.NextPage == 0 {
			break
		}
		memberOptions.Page = resp.NextPage
	}

	log.Printf("[DEBUG] Reading team maintainers: %s", teamIdString)
	maintainerOptions := github.TeamListTeamMembersOptions{
		ListOptions: github.ListOptions{
			PerPage: maxPerPage,
		},
		Role: "maintainer",
	}
	var maintainers []*github.User
	for {
		maintaner, resp, err := client.Teams.ListTeamMembersByID(ctx, orgId, teamId, &maintainerOptions)
		if err != nil {
			return err
		}

		etags = append(etags, resp.Header.Get("ETag"))
		maintainers = append(maintainers, maintaner...)

		if resp.NextPage == 0 {
			break
		}
		maintainerOptions.Page = resp.NextPage
	}

	teamMembersAndMaintainers := make([]interface{}, len(members)+len(maintainers))
	// Add all members to the list
	for i, member := range members {
		teamMembersAndMaintainers[i] = map[string]interface{}{
			"username": member.Login,
			"role":     "member",
		}
	}
	// Add all maintainers to the list
	for i, member := range maintainers {
		teamMembersAndMaintainers[i+len(members)] = map[string]interface{}{
			"username": member.Login,
			"role":     "maintainer",
		}
	}

	if err := d.Set("members", teamMembersAndMaintainers); err != nil {
		return err
	}

	// Combine etag of all requests
	d.Set("etag", buildChecksumID(etags))

	return nil
}

func resourceGithubTeamMembersDelete(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*Owner).v3client
	orgId := meta.(*Owner).id
	teamIdString := d.Get("team_id").(string)
	teamId, err := getTeamID(teamIdString, meta)
	if err != nil {
		return err
	}

	members := d.Get("members").(*schema.Set)
	ctx := context.WithValue(context.Background(), ctxId, d.Id())

	for _, member := range members.List() {
		mem := member.(map[string]interface{})
		username := mem["username"].(string)

		log.Printf("[DEBUG] Deleting team membership: %s/%s", teamIdString, username)

		_, err = client.Teams.RemoveTeamMembershipByID(ctx, orgId, teamId, username)
		if err != nil {
			return err
		}
	}

	return nil
}

// Package provider implements the sqlmigrate Terraform provider.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"

	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/gcp"
)

// New returns a provider factory.
func New(version string) func() provider.Provider {
	return func() provider.Provider { return &sqlmigrateProvider{version: version} }
}

type sqlmigrateProvider struct{ version string }

type providerModel struct {
	Project         types.String `tfsdk:"project"`
	Region          types.String `tfsdk:"region"`
	AccessToken     types.String `tfsdk:"access_token"`
	Credentials     types.String `tfsdk:"credentials"`
	StorageEndpoint types.String `tfsdk:"storage_endpoint"`
	RunEndpoint     types.String `tfsdk:"run_endpoint"`
	PollInterval    types.String `tfsdk:"poll_interval"`
}

// client is shared by every resource and data source.
type client struct {
	storage *gcp.Storage
	run     *gcp.Run
	project string
	region  string
	poll    time.Duration
}

func (p *sqlmigrateProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "sqlmigrate"
	resp.Version = p.version
}

func (p *sqlmigrateProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Runs Flyway SQL migrations through a Cloud Run job inside the database's network, " +
			"and shows pending migrations at plan time from a manifest the job keeps in Cloud Storage.",
		Attributes: map[string]schema.Attribute{
			"project": schema.StringAttribute{
				Optional:    true,
				Description: "Project used when `job` is a short name. Defaults to GOOGLE_PROJECT or GOOGLE_CLOUD_PROJECT.",
			},
			"region": schema.StringAttribute{
				Optional:    true,
				Description: "Region used when `job` is a short name. Defaults to GOOGLE_REGION.",
			},
			"access_token": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				Description: "OAuth access token, for example `data.google_client_config.default.access_token`. " +
					"Defaults to GOOGLE_OAUTH_ACCESS_TOKEN. Takes precedence over `credentials`.",
			},
			"credentials": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				Description: "Path to, or contents of, a credentials file: a service account key, or an external_account " +
					"(workload identity federation) configuration. Defaults to GOOGLE_CREDENTIALS, then Application Default Credentials.",
			},
			"storage_endpoint": schema.StringAttribute{
				Optional:    true,
				Description: "Cloud Storage JSON API base URL. Defaults to https://storage.googleapis.com/storage/v1/.",
			},
			"run_endpoint": schema.StringAttribute{
				Optional:    true,
				Description: "Cloud Run Admin API base URL. Defaults to https://run.googleapis.com/.",
			},
			"poll_interval": schema.StringAttribute{
				Optional:    true,
				Description: "How often to check a running job, as a Go duration. Defaults to 5s.",
			},
		},
	}
}

func (p *sqlmigrateProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	for _, a := range []struct {
		name string
		v    types.String
	}{{"access_token", cfg.AccessToken}, {"credentials", cfg.Credentials}, {"storage_endpoint", cfg.StorageEndpoint}, {"run_endpoint", cfg.RunEndpoint}} {
		if a.v.IsUnknown() {
			// Not known until apply (for example, a token from a resource
			// being created). Configure runs again once it is known.
			resp.Diagnostics.AddAttributeWarning(path.Root(a.name), "Provider configuration not yet known",
				a.name+" is unknown during this plan, so sqlmigrate resources cannot read their manifests yet.")
			return
		}
	}

	c := &client{
		project: firstNonEmpty(cfg.Project.ValueString(), os.Getenv("GOOGLE_PROJECT"), os.Getenv("GOOGLE_CLOUD_PROJECT")),
		region:  firstNonEmpty(cfg.Region.ValueString(), os.Getenv("GOOGLE_REGION")),
		poll:    5 * time.Second,
	}
	if s := cfg.PollInterval.ValueString(); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			resp.Diagnostics.AddAttributeError(path.Root("poll_interval"), "Invalid poll_interval", fmt.Sprintf("%q is not a positive Go duration such as 5s.", s))
			return
		}
		c.poll = d
	}

	common := []option.ClientOption{option.WithUserAgent("github.com/jq6l43d1/terraform-provider-sqlmigrate/" + p.version)}
	token := firstNonEmpty(cfg.AccessToken.ValueString(), os.Getenv("GOOGLE_OAUTH_ACCESS_TOKEN"))
	creds := firstNonEmpty(cfg.Credentials.ValueString(), os.Getenv("GOOGLE_CREDENTIALS"))
	switch {
	case token != "":
		common = append(common, option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})))
	case creds != "":
		opt, err := credentialsOption(creds)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("credentials"), "Invalid credentials", err.Error())
			return
		}
		common = append(common, opt, option.WithScopes("https://www.googleapis.com/auth/cloud-platform"))
	default:
		common = append(common, option.WithScopes("https://www.googleapis.com/auth/cloud-platform"))
	}

	storageOpts := append([]option.ClientOption{}, common...)
	if ep := cfg.StorageEndpoint.ValueString(); ep != "" {
		storageOpts = append(storageOpts, option.WithEndpoint(ep))
	}
	runOpts := append([]option.ClientOption{}, common...)
	if ep := cfg.RunEndpoint.ValueString(); ep != "" {
		runOpts = append(runOpts, option.WithEndpoint(ep))
	}

	var err error
	if c.storage, err = gcp.NewStorage(ctx, storageOpts...); err != nil {
		resp.Diagnostics.AddError("Cannot create Cloud Storage client", err.Error())
		return
	}
	if c.run, err = gcp.NewRun(ctx, runOpts...); err != nil {
		resp.Diagnostics.AddError("Cannot create Cloud Run client", err.Error())
		return
	}
	resp.ResourceData = c
	resp.DataSourceData = c
}

// credentialsOption accepts a file path or JSON contents and picks the
// loader for the file's "type".
func credentialsOption(v string) (option.ClientOption, error) {
	data := []byte(v)
	if !strings.HasPrefix(strings.TrimSpace(v), "{") {
		b, err := os.ReadFile(v)
		if err != nil {
			return nil, fmt.Errorf("reading credentials file: %w", err)
		}
		data = b
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("credentials are not valid JSON: %w", err)
	}
	credTypes := map[string]option.CredentialsType{
		"service_account":              option.ServiceAccount,
		"authorized_user":              option.AuthorizedUser,
		"external_account":             option.ExternalAccount,
		"impersonated_service_account": option.ImpersonatedServiceAccount,
	}
	t, ok := credTypes[head.Type]
	if !ok {
		return nil, fmt.Errorf("unsupported credentials type %q", head.Type)
	}
	return option.WithAuthCredentialsJSON(t, data), nil
}

func (p *sqlmigrateProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{newMigrationsResource}
}

func (p *sqlmigrateProvider) DataSources(context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{newManifestDataSource}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// jobName expands a short job name with the provider's project and region.
func (c *client) jobName(job string) (string, error) {
	if strings.HasPrefix(job, "projects/") {
		parts := strings.Split(job, "/")
		if len(parts) != 6 || parts[2] != "locations" || parts[4] != "jobs" {
			return "", fmt.Errorf("job %q must be projects/PROJECT/locations/REGION/jobs/NAME or a short name", job)
		}
		return job, nil
	}
	if strings.Contains(job, "/") {
		return "", fmt.Errorf("job %q must be projects/PROJECT/locations/REGION/jobs/NAME or a short name", job)
	}
	if c.project == "" || c.region == "" {
		return "", fmt.Errorf("job %q is a short name, so the provider needs project and region (or GOOGLE_PROJECT and GOOGLE_REGION)", job)
	}
	return fmt.Sprintf("projects/%s/locations/%s/jobs/%s", c.project, c.region, job), nil
}

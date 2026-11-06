package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/manifest"
)

var _ datasource.DataSourceWithConfigure = (*manifestDataSource)(nil)

func newManifestDataSource() datasource.DataSource { return &manifestDataSource{} }

type manifestDataSource struct{ client *client }

type manifestModel struct {
	Bucket        types.String `tfsdk:"bucket"`
	Prefix        types.String `tfsdk:"prefix"`
	Exists        types.Bool   `tfsdk:"exists"`
	RunID         types.String `tfsdk:"run_id"`
	Command       types.String `tfsdk:"command"`
	Status        types.String `tfsdk:"status"`
	ErrorMessage  types.String `tfsdk:"error_message"`
	FinishedAt    types.String `tfsdk:"finished_at"`
	FlywayVersion types.String `tfsdk:"flyway_version"`
	DatabaseType  types.String `tfsdk:"database_type"`
	SchemaVersion types.String `tfsdk:"schema_version"`
	Applied       types.List   `tfsdk:"applied"`
}

var appliedObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"category":     types.StringType,
	"version":      types.StringType,
	"description":  types.StringType,
	"type":         types.StringType,
	"state":        types.StringType,
	"installed_on": types.StringType,
	"script":       types.StringType,
	"checksum":     types.Int64Type,
}}

func (d *manifestDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_manifest"
}

func (d *manifestDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	str := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, Description: desc}
	}
	resp.Schema = schema.Schema{
		Description: "Reads the manifest the runner keeps for one database: the latest run and the schema history it saw.",
		Attributes: map[string]schema.Attribute{
			"bucket":         schema.StringAttribute{Required: true, Description: "Bucket holding the manifest."},
			"prefix":         schema.StringAttribute{Required: true, Description: "Prefix of the database's objects."},
			"exists":         schema.BoolAttribute{Computed: true, Description: "Whether a manifest has been written yet."},
			"run_id":         str("Run id of the latest run that read the schema history."),
			"command":        str("Command of that run."),
			"status":         str("`succeeded` or `failed`."),
			"error_message":  str("Error from that run, if it failed."),
			"finished_at":    str("When that run finished (RFC 3339)."),
			"flyway_version": str("Flyway version used."),
			"database_type":  str("Database type Flyway detected."),
			"schema_version": str("Highest applied version."),
			"applied": schema.ListAttribute{
				Computed:    true,
				ElementType: appliedObjectType,
				Description: "Current schema history rows: category, version, description, type, state, installed_on, script and checksum (null when unknown).",
			},
		},
	}
}

func (d *manifestDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("got %T", req.ProviderData))
		return
	}
	d.client = c
}

func (d *manifestDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m manifestModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if d.client == nil {
		resp.Diagnostics.AddError("Provider not configured", "The sqlmigrate provider configuration is not known yet.")
		return
	}
	prefix, err := manifest.CleanPrefix(m.Prefix.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("prefix"), "Invalid prefix", err.Error())
		return
	}
	rec, err := readRecord(ctx, d.client, m.Bucket.ValueString(), manifest.ManifestObject(prefix))
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the migration manifest", manifestHint(err, m.Bucket.ValueString(), prefix))
		return
	}

	m.Exists = types.BoolValue(rec != nil)
	m.RunID, m.Command, m.Status, m.ErrorMessage = types.StringNull(), types.StringNull(), types.StringNull(), types.StringNull()
	m.FinishedAt, m.FlywayVersion, m.DatabaseType, m.SchemaVersion = types.StringNull(), types.StringNull(), types.StringNull(), types.StringNull()
	rows := []attr.Value{}
	if rec != nil {
		m.RunID = optString(rec.RunID)
		m.Command = optString(rec.Command)
		m.Status = optString(rec.Status)
		if rec.Error != nil {
			m.ErrorMessage = optString(rec.Error.Message)
		}
		if !rec.FinishedAt.IsZero() {
			m.FinishedAt = types.StringValue(rec.FinishedAt.UTC().Format(time.RFC3339))
		}
		m.FlywayVersion = optString(rec.FlywayVersion)
		m.DatabaseType = optString(rec.DatabaseType)
		m.SchemaVersion = optString(rec.SchemaVersion)
		for _, a := range rec.Applied {
			checksum := types.Int64Null()
			if a.Checksum != nil {
				checksum = types.Int64Value(int64(*a.Checksum))
			}
			obj, diags := types.ObjectValue(appliedObjectType.AttrTypes, map[string]attr.Value{
				"category":     types.StringValue(a.Category),
				"version":      types.StringValue(a.Version),
				"description":  types.StringValue(a.Description),
				"type":         types.StringValue(a.Type),
				"state":        types.StringValue(a.State),
				"installed_on": types.StringValue(a.InstalledOn),
				"script":       types.StringValue(a.Script),
				"checksum":     checksum,
			})
			resp.Diagnostics.Append(diags...)
			rows = append(rows, obj)
		}
	}
	list, diags := types.ListValue(appliedObjectType, rows)
	resp.Diagnostics.Append(diags...)
	m.Applied = list
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

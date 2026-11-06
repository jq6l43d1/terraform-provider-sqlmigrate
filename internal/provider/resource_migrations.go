package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/bundle"
	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/gcp"
	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/manifest"
	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/migrations"
	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/reconcile"
	"github.com/jq6l43d1/terraform-provider-sqlmigrate/internal/runner"
)

const defaultTimeout = 30 * time.Minute

var (
	_ resource.ResourceWithConfigure   = (*migrationsResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*migrationsResource)(nil)
	_ resource.ResourceWithImportState = (*migrationsResource)(nil)
)

func newMigrationsResource() resource.Resource { return &migrationsResource{} }

type migrationsResource struct{ client *client }

type migrationsModel struct {
	ID            types.String   `tfsdk:"id"`
	SourceDir     types.String   `tfsdk:"source_dir"`
	Bucket        types.String   `tfsdk:"bucket"`
	Prefix        types.String   `tfsdk:"prefix"`
	Job           types.String   `tfsdk:"job"`
	Container     types.String   `tfsdk:"container"`
	Placeholders  types.Map      `tfsdk:"placeholders"`
	Triggers      types.Map      `tfsdk:"triggers"`
	Applied       types.List     `tfsdk:"applied"`
	SchemaVersion types.String   `tfsdk:"schema_version"`
	LastRunID     types.String   `tfsdk:"last_run_id"`
	LastExecution types.String   `tfsdk:"last_execution"`
	Timeouts      timeouts.Value `tfsdk:"timeouts"`
}

func (r *migrationsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_migrations"
}

func (r *migrationsResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Applies the Flyway migrations in `source_dir` by running a Cloud Run job, and waits for it. " +
			"The plan lists pending migrations as additions to `applied`; nothing runs when nothing is pending. " +
			"Destroying the resource leaves the database unchanged.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "`<bucket>/<prefix>`.",
			},
			"source_dir": schema.StringAttribute{
				Required: true,
				Description: "Directory of Flyway migrations (`V<version>__<description>.sql`, `R__<description>.sql`), " +
					"usually `\"${path.module}/migrations\"`. Every `.sql` and `.sql.conf` file under it is shipped to the job.",
			},
			"bucket": schema.StringAttribute{
				Required:    true,
				Description: "Cloud Storage bucket the job reads bundles from and writes its manifest to.",
			},
			"prefix": schema.StringAttribute{
				Required:    true,
				Description: "Object prefix for this database in the bucket. Must match the job's SQLMIGRATE_PREFIX.",
			},
			"job": schema.StringAttribute{
				Required:    true,
				Description: "Cloud Run job running the sqlmigrate runner: `projects/P/locations/L/jobs/NAME`, or NAME with the provider's project and region.",
			},
			"container": schema.StringAttribute{
				Optional:    true,
				Description: "Container to override when the job has more than one.",
			},
			"placeholders": schema.MapAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Flyway placeholders passed to each run, e.g. `{ app_role = \"app_rw\" }` for `${app_role}` in SQL. " +
					"Changing them does not re-run applied migrations.",
			},
			"triggers": schema.MapAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Any change to this map runs the job even when nothing is pending, for example to refresh the manifest after a manual repair.",
			},
			"applied": schema.ListAttribute{
				ElementType: types.StringType,
				Computed:    true,
				Description: "Migrations from `source_dir` that are applied, in run order: `\"<version>: <description>\"` for versioned " +
					"and `\"R: <description> (checksum N)\"` for repeatable migrations.",
			},
			"schema_version": schema.StringAttribute{
				Computed:    true,
				Description: "Highest applied version, as Flyway reports it.",
			},
			"last_run_id": schema.StringAttribute{
				Computed:    true,
				Description: "Run id of the last job execution this resource started. Its record is at `<prefix>/runs/<id>.json`.",
			},
			"last_execution": schema.StringAttribute{
				Computed:    true,
				Description: "Cloud Run execution name of the last run.",
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx, timeouts.Opts{Create: true, Update: true}),
		},
	}
}

func (r *migrationsResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("got %T", req.ProviderData))
		return
	}
	r.client = c
}

// ModifyPlan reads the manifest and works out what the run will do. This is
// where pending migrations become visible in `terraform plan`.
func (r *migrationsResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroy
	}
	var plan migrationsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var prior *migrationsModel
	if !req.State.Raw.IsNull() {
		var s migrationsModel
		resp.Diagnostics.Append(req.State.Get(ctx, &s)...)
		if resp.Diagnostics.HasError() {
			return
		}
		prior = &s
	}

	if plan.Bucket.IsUnknown() || plan.Prefix.IsUnknown() || plan.SourceDir.IsUnknown() || r.client == nil {
		if plan.Bucket.IsUnknown() || plan.Prefix.IsUnknown() {
			plan.ID = types.StringUnknown()
		}
		plan.Applied = types.ListUnknown(types.StringType)
		plan.SchemaVersion = types.StringUnknown()
		plan.LastRunID = types.StringUnknown()
		plan.LastExecution = types.StringUnknown()
		resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
		return
	}

	bucket := plan.Bucket.ValueString()
	prefix, err := manifest.CleanPrefix(plan.Prefix.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("prefix"), "Invalid prefix", err.Error())
		return
	}
	plan.ID = types.StringValue(bucket + "/" + prefix)

	src, err := migrations.Load(plan.SourceDir.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("source_dir"), "Cannot read migrations", err.Error())
		return
	}
	for _, w := range src.Warnings {
		resp.Diagnostics.AddAttributeWarning(path.Root("source_dir"), "File ignored by Flyway", w)
	}
	rec, err := readRecord(ctx, r.client, bucket, manifest.ManifestObject(prefix))
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the migration manifest", manifestHint(err, bucket, prefix))
		return
	}
	ev := reconcile.Evaluate(src.Migrations, rec)
	for _, e := range ev.Errors {
		resp.Diagnostics.AddAttributeError(path.Root("source_dir"), "Migration cannot be applied", e)
	}
	for _, w := range ev.Warnings {
		resp.Diagnostics.AddAttributeWarning(path.Root("source_dir"), "Migration warning", w)
	}
	if resp.Diagnostics.HasError() {
		return
	}

	applied, d := types.ListValueFrom(ctx, types.StringType, ev.Expected)
	resp.Diagnostics.Append(d...)
	plan.Applied = applied

	needRun := len(ev.Pending) > 0
	if prior != nil && !prior.Triggers.Equal(plan.Triggers) {
		needRun = true
	}
	if needRun {
		var names []string
		for _, m := range ev.Pending {
			names = append(names, m.Path)
		}
		tflog.Info(ctx, "migrations pending", map[string]any{"pending": names})
		plan.SchemaVersion = types.StringUnknown()
		plan.LastRunID = types.StringUnknown()
		plan.LastExecution = types.StringUnknown()
	} else {
		plan.SchemaVersion = types.StringNull()
		plan.LastRunID = types.StringNull()
		plan.LastExecution = types.StringNull()
		if prior != nil {
			plan.SchemaVersion = prior.SchemaVersion
			plan.LastRunID = prior.LastRunID
			plan.LastExecution = prior.LastExecution
		}
		if rec != nil && rec.HistoryRead {
			plan.SchemaVersion = optString(rec.SchemaVersion)
		}
	}
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

func (r *migrationsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan migrationsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, d := plan.Timeouts.Create(ctx, defaultTimeout)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	state := r.apply(ctx, plan, nil, timeout, &resp.Diagnostics)
	if resp.Diagnostics.HasError() && state.LastRunID.IsNull() {
		return // nothing ran; leave the resource uncreated rather than tainted
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *migrationsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior migrationsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, d := plan.Timeouts.Update(ctx, defaultTimeout)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	state := r.apply(ctx, plan, &prior, timeout, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// apply runs the job when the plan expects a run (last_run_id unknown) and
// returns the new state. On failure it returns what is actually applied, so
// the next plan starts from the truth.
func (r *migrationsResource) apply(ctx context.Context, plan migrationsModel, prior *migrationsModel, timeout time.Duration, diags *diag.Diagnostics) migrationsModel {
	state := plan
	if !plan.LastRunID.IsUnknown() {
		return state // nothing to run
	}
	if r.client == nil {
		diags.AddError("Provider not configured", "The sqlmigrate provider configuration was not available during apply.")
		return state
	}

	// On any failure below, keep what was known before the run.
	state.Applied = types.ListValueMust(types.StringType, nil)
	state.SchemaVersion = types.StringNull()
	state.LastRunID = types.StringNull()
	state.LastExecution = types.StringNull()
	if prior != nil {
		state.Applied = prior.Applied
		state.SchemaVersion = prior.SchemaVersion
		state.LastRunID = prior.LastRunID
		state.LastExecution = prior.LastExecution
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	job, err := r.client.jobName(plan.Job.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("job"), "Invalid job", err.Error())
		return state
	}
	bucket := plan.Bucket.ValueString()
	prefix, err := manifest.CleanPrefix(plan.Prefix.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("prefix"), "Invalid prefix", err.Error())
		return state
	}
	src, err := migrations.Load(plan.SourceDir.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("source_dir"), "Cannot read migrations", err.Error())
		return state
	}
	uploaded, err := bundle.Upload(ctx, r.client.storage, bucket, prefix, src, time.Now())
	if err != nil {
		diags.AddError("Cannot upload the migration bundle", err.Error())
		return state
	}
	tflog.Info(ctx, "bundle ready", map[string]any{"bundle": src.BundleID, "uploaded": uploaded})

	env := map[string]string{
		runner.EnvRunID:   newRunID(),
		runner.EnvBundle:  src.BundleID,
		runner.EnvCommand: manifest.CommandMigrate,
	}
	if !plan.Placeholders.IsNull() && len(plan.Placeholders.Elements()) > 0 {
		ph := map[string]string{}
		diags.Append(plan.Placeholders.ElementsAs(ctx, &ph, false)...)
		if diags.HasError() {
			return state
		}
		b, _ := json.Marshal(ph)
		env[runner.EnvPlaceholders] = string(b)
	}
	runID := env[runner.EnvRunID]

	op, execution, err := r.client.run.RunJob(ctx, job, plan.Container.ValueString(), env)
	if err != nil {
		diags.AddError("Cannot start the migration job", err.Error()+
			"\n\nThe identity running Terraform needs run.jobs.runWithOverrides on the job (roles/run.developer, or a custom role).")
		return state
	}
	state.LastRunID = types.StringValue(runID)
	state.LastExecution = types.StringValue(execution)
	tflog.Info(ctx, "migration job started", map[string]any{"execution": execution, "run_id": runID})

	result, err := r.client.run.Wait(ctx, op, execution, r.client.poll)
	if err != nil {
		diags.AddError("Lost track of the migration job", err.Error())
		return state
	}
	if result.Name != "" {
		state.LastExecution = types.StringValue(result.Name)
	}

	rec, err := readRecord(ctx, r.client, bucket, manifest.RunObject(prefix, runID))
	if err != nil {
		diags.AddError("Cannot read the run record", err.Error())
		return state
	}
	var ev reconcile.Result
	if rec != nil && rec.HistoryRead {
		ev = reconcile.Evaluate(src.Migrations, rec)
		applied, d := types.ListValueFrom(ctx, types.StringType, ev.Applied)
		diags.Append(d...)
		state.Applied = applied
		state.SchemaVersion = optString(rec.SchemaVersion)
	}

	if !result.Succeeded || rec == nil || rec.Status != manifest.StatusSucceeded {
		diags.AddError("Migration run failed", describeFailure(rec, result, runID, bucket, prefix))
		return state
	}
	if len(ev.Pending) > 0 {
		var names []string
		for _, m := range ev.Pending {
			names = append(names, m.Path)
		}
		diags.AddError("Migrations still pending after the run",
			"The job succeeded but these migrations are not applied: "+strings.Join(names, ", ")+
				". Check that the job does not set FLYWAY_TARGET or FLYWAY_OUT_OF_ORDER settings that skip them.")
		return state
	}
	state.Applied = plan.Applied
	return state
}

func (r *migrationsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state migrationsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || r.client == nil {
		return
	}
	prefix, err := manifest.CleanPrefix(state.Prefix.ValueString())
	if err != nil {
		return
	}
	rec, err := readRecord(ctx, r.client, state.Bucket.ValueString(), manifest.ManifestObject(prefix))
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the migration manifest", manifestHint(err, state.Bucket.ValueString(), prefix))
		return
	}
	if rec == nil || !rec.HistoryRead {
		state.Applied = types.ListValueMust(types.StringType, nil)
		state.SchemaVersion = types.StringNull()
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		return
	}
	var src []migrations.Migration
	if !state.SourceDir.IsNull() && state.SourceDir.ValueString() != "" {
		loaded, err := migrations.Load(state.SourceDir.ValueString())
		if err != nil {
			// The plan reports this properly; keep the recorded list.
			resp.Diagnostics.AddAttributeWarning(path.Root("source_dir"), "Cannot read migrations during refresh", err.Error())
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}
		src = loaded.Migrations
	}
	ev := reconcile.Evaluate(src, rec)
	applied, d := types.ListValueFrom(ctx, types.StringType, ev.Applied)
	resp.Diagnostics.Append(d...)
	state.Applied = applied
	state.SchemaVersion = optString(rec.SchemaVersion)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *migrationsResource) Delete(_ context.Context, _ resource.DeleteRequest, resp *resource.DeleteResponse) {
	resp.Diagnostics.AddWarning("Database left unchanged",
		"Removing sqlmigrate_migrations does not undo migrations or touch the database. "+
			"The bundles, run records and manifest stay in the bucket.")
}

// ImportState takes "<bucket>/<prefix>".
func (r *migrationsResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	bucket, prefix, ok := strings.Cut(req.ID, "/")
	if !ok || bucket == "" {
		resp.Diagnostics.AddError("Invalid import id", "Use <bucket>/<prefix>, for example my-bucket/app/dev.")
		return
	}
	clean, err := manifest.CleanPrefix(prefix)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import id", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), bucket+"/"+clean)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("bucket"), bucket)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("prefix"), clean)...)
}

func readRecord(ctx context.Context, c *client, bucket, object string) (*manifest.Record, error) {
	data, found, err := c.storage.Get(ctx, bucket, object)
	if err != nil || !found {
		return nil, err
	}
	return manifest.Decode(data)
}

func manifestHint(err error, bucket, prefix string) string {
	return fmt.Sprintf("%v\n\nThe identity running terraform plan needs storage.objects.get on gs://%s (the manifest is %s).",
		err, bucket, manifest.ManifestObject(prefix))
}

func describeFailure(rec *manifest.Record, ex *gcp.Execution, runID, bucket, prefix string) string {
	var b strings.Builder
	switch {
	case rec == nil:
		b.WriteString("The job finished without writing a run record, so it probably failed before it could reach Cloud Storage " +
			"(bad configuration, missing permissions, or the image could not start). Check the execution's logs.\n")
	case rec.Error != nil:
		where := ""
		if rec.Error.Script != "" {
			where = " in " + rec.Error.Script
			if rec.Error.Line > 0 {
				where += fmt.Sprintf(" at line %d", rec.Error.Line)
			}
		}
		fmt.Fprintf(&b, "Flyway %s failed%s (%s):\n\n%s\n", rec.Command, where, rec.Error.Code, indent(rec.Error.Message))
		switch rec.Error.Code {
		case "VALIDATE_ERROR":
			b.WriteString("\nValidation fails when an applied migration was edited, renamed or removed. Restore it and add a new migration instead.\n")
		case "FAILED_VERSIONED_MIGRATION", "FAILED_REPEATABLE_MIGRATION":
			b.WriteString("\nMigrations before the failing one were applied. On databases without transactional DDL " +
				"(MySQL, for example) the failed migration may be half applied and needs a manual fix and a repair run.\n")
		}
	default:
		b.WriteString("The run record says the run did not succeed.\n")
	}
	if rec != nil && len(rec.Executed) > 0 {
		b.WriteString("\nApplied during this run:\n")
		for _, e := range rec.Executed {
			label := e.Version
			if label == "" {
				label = "R"
			}
			fmt.Fprintf(&b, "  %s: %s\n", label, e.Description)
		}
	}
	fmt.Fprintf(&b, "\nExecution: %s", ex.Name)
	if !ex.Succeeded && ex.Message != "" {
		fmt.Fprintf(&b, "\nCloud Run: %s", ex.Message)
	}
	if ex.LogURI != "" {
		fmt.Fprintf(&b, "\nLogs: %s", ex.LogURI)
	}
	fmt.Fprintf(&b, "\nRun record: gs://%s/%s", bucket, manifest.RunObject(prefix, runID))
	return b.String()
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n  ")
}

func optString(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}

func newRunID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "tf-" + time.Now().UTC().Format("20060102t150405") + "-" + hex.EncodeToString(b)
}

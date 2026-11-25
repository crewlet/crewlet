# AWS

Each agent gets its **own IAM role** in your account, at the level you choose, signed in 15 minutes at a time, so CloudTrail names the agent behind every call. Agents use the **AWS CLI** in their box and **AWS's MCP Server**, both as that role.

Connecting creates one CloudFormation stack. Crewlet stores no access key: it signs in through **IAM Roles Anywhere** with a certificate authority it holds, whose certificate the stack registers. Events from AWS are not received yet ([What is not supported yet](#what-is-not-supported-yet)).

## What agents get

| An agent on | Can | Cannot |
|---|---|---|
| `read` (the default) | Describe and list EC2 (with VPC), Auto Scaling, Elastic Load Balancing, ECS, EKS, RDS, Route 53, CloudFront; read how a Lambda function is wired (aliases, event sources, URL, policy, concurrency), but not list functions or read a function's configuration; read CloudWatch metrics and Logs; list bucket and object names; read Systems Manager inventory and parameter names; look up CloudTrail events; read ECR, ACM and KMS metadata, tags and AWS Health | Read objects, items, parameter values or secrets; change anything |
| `operate` | `read`, plus start, stop, reboot instances; scale Auto Scaling groups, ECS services, EKS node groups; invoke functions; reboot, start, stop, snapshot databases; read, write, delete S3 objects; register load balancer targets; invalidate CloudFront; edit CloudWatch alarms and dashboards; read plain Systems Manager parameters | Create key pairs, user data, database passwords, bucket or key policies, DNS records, security group changes, new code or task definitions |
| `administer` | `operate`, plus manage those services whole | See below |

**Administer can create access that outlives the session:** instance key pairs, user data, database users, bucket and key policies, function URLs, security group openings, snapshot sharing. Granting it takes a fresh sign-in. Moving an agent off Administer leaves what it created; review and remove it.

**No level can ever** (the stack's permissions boundary refuses it): write IAM or pass a role, assume another role or mint another credential, read a secret's value, manage KMS keys, touch billing, the organization or the account, administer an EKS cluster's access, stop or change CloudTrail. Agents work only in the company's Regions ([Regions](#regions)).

**Acting as a workload's identity is gated.** Starting a shell or command on an instance (Session Manager, EC2 Instance Connect), running an ECS task or command, or changing a function's code uses that workload's own role, so it is allowed only on resources tagged `crewlet:agent-access = administer`, and only at Administer.

**Read includes CloudWatch Logs**, so a secret a workload writes to logs, user data or a task definition's environment variables is readable at Read. A function's environment variables are not: Read cannot list functions or read their configuration (both expose the environment); it finds a function by log group or tags, and reads configuration only at Operate.

The company-wide **Let agents read IAM** option adds `iam:Get*` and `iam:List*` to every agent that is on. Off by default, since it shows the people in your account. Each agent's role is its own, so turning one agent off never touches another.

## Setting it up from the dashboard

Company › Integrations › AWS asks, in order:

1. **Short name**: the name on every Crewlet object in your account, as `crewlet-<name>`. Offered from the company's name; up to 20 lowercase letters, digits and hyphens; fixed once the stack exists.
2. **Home Region**: where the stack and Crewlet's sign-in live; any Region IAM Roles Anywhere is available in.
3. **Let agents read IAM**: the option above, off by default.
4. **See more in Atlas**: optional, off by default. Adds a read-only role for the atlas ([what the atlas reads](#what-the-atlas-reads)); no agent depends on it.

**Connect** mints the **sign-in certificate**: a certificate authority whose key Crewlet seals as `AWS_CERTIFICATE_AUTHORITY` and never shows. The form then shows the **CloudShell block**, which carries only the authority's certificate, with a **Copy** button.

5. Copy the block whole with its **Copy** button (a hand selection can drop a line). In the AWS account agents will work in, signed in as an administrator, open CloudShell in the home Region and paste it. It writes the template, deploys it with `aws cloudformation deploy` (you need permission to create IAM roles and policies), and prints the stack's one output last: five ARNs joined by commas, or seven with the Atlas reader.
   - If it prints `Failed to create/update the stack`, the stack's **Events** tab in CloudFormation says why. Fix that and run the block again; delete a stack left in `ROLLBACK_COMPLETE` first.
   - Ran it in the wrong account? Delete the `crewlet-<name>` stack there in CloudFormation, which removes everything it made.
6. Paste that last line into **Stack output**, press **Test connection**, then **Save**. The form closes, and within about a minute the status goes to **Connected**.
7. **Connected gives no agent access yet.** On the **Agents** tab, turn **On** each agent that should use AWS, choose its **Access** level, then press **Review changes** and **Save and sync** ([giving each agent its own role](#giving-each-agent-its-own-role)).

Running the block again updates the stack in place; that is how new Crewlet policies, a change of Regions, or the atlas's role reach your account. Paste the last line again whenever the block changed what the stack holds. The short name, home Region and atlas choice save on their own while **Stack output** is empty; the form keeps the block drawn from what is saved and says when a change means running it again.

### What the stack creates

| Object | Name | What it is for |
|---|---|---|
| Stack | `crewlet-<name>` | Holds everything below; deleting it removes Crewlet's access |
| Trust anchor | `crewlet-<name>` | Holds Crewlet's certificate |
| Provisioner role and profile | `crewlet/<name>/crewlet-<name>-provisioner` | Creates, levels, turns off, deletes agents' roles and attaches granted AWS managed policies, only inside the boundary |
| Broker role and profile | `crewlet/<name>/crewlet-<name>-broker` | Becomes one agent's role at a time, nothing else |
| Permissions boundary | `crewlet-<name>-seat-boundary` | The most any agent role can ever do |
| Level policies | `crewlet-<name>-level-read`, `-level-operate`, `-level-administer`, `-level-iam-read` | What each level grants |
| Atlas role and profile (only with **See more in Atlas**) | `crewlet/<name>/crewlet-<name>-atlas` | Lists what the account runs, read-only |

Every object is tagged `crewlet:managed-by = crewlet`, `crewlet:company = <name>` and `crewlet:stack-version`. Agents' roles live under `/crewlet/<name>/seat/`.

Each certificate can become only its own role: the atlas's only the atlas role, which neither the provisioner nor the broker can become; Crewlet's for the provisioner only the provisioner, which cannot become any agent; an agent's sign-in only the broker, which can become only that agent's role (each role trusts the broker only in a session named for that agent).

### Testing before you save

**Test connection** signs in as the provisioner and reads, changing nothing; a failure lists each part Passed up to the one that Failed:

| Check | What a failure means |
|---|---|
| The sign-in | The trust anchor lacks this engine's certificate, or the output is another stack's |
| The stack's version | The stack was made by another Crewlet version |
| Every policy and role | The boundary, a level, or the provisioner's or broker's trust or policy differs from what this engine renders: edited by hand, or the Regions changed |
| The identity split | An agent's certificate could become the provisioner, or the provisioner's the broker; with the atlas's role, its certificate could become either, or either's the atlas |
| The atlas's role (only with **See more in Atlas**) | The stack or pasted output lacks the role, or its trust or policy differs. A warning beside a working connection, never a refusal |

The fix is always the same: run the CloudShell block again; never edit Crewlet's roles or policies by hand. When all pass, the result gives the account's IAM role count against its quota and warns when fewer than 20 roles are left. Every later pass repeats the check; a difference is a finding on the integration's card.

## What the atlas reads

By default the [atlas](../concepts/atlas.md) shows the AWS account and the agents' Regions from settings alone. **See more in Atlas** lets it list what the account runs. It never changes what an agent can do.

**The one step.** Turn **See more in Atlas** on, save, run the CloudShell block again and paste its last line into **Stack output**. The block now creates the atlas's role and profile, and the last line holds seven ARNs where it held five. In the company document the switch is `integrations.aws.provisioning.atlas_read`.

**To undo it.** Turn the switch off, save, run the block again: CloudFormation deletes the role and profile. The switch stops the reads at once, before the block is run; deleting the role in IAM by hand also stops them, and the atlas's Sources tab says the read failed. What the atlas recorded stays until a person retires it.

**Who reads.** The engine signs in as the role `crewlet-<name>-atlas` through the same trust anchor, with a certificate named for the atlas, once an hour unless `atlas.refresh_interval_seconds` says otherwise, and on **Refresh now**. CloudTrail shows these calls under that role, source identity `CN=crewlet-<name>-atlas`, never under an agent's.

**What the role may do.** Its policy allows nine actions and denies every other, so a hand-attached policy widens nothing:

| Action | What the atlas gains |
|---|---|
| `ec2:DescribeRegions` | The enabled Regions, each then asked |
| `ec2:DescribeInstances` | Each instance as a server: id, `Name` tag, private and public addresses, Region. Terminated instances are left out |
| `rds:DescribeDBInstances` | Each database instance: identifier, engine (`postgres`, `mysql`, `mariadb` or `other`), version, port |
| `elasticloadbalancing:DescribeLoadBalancers` | Each application, network and gateway load balancer as the host name a DNS record points at. Classic Load Balancers are not listed |
| `sqs:ListQueues` | Each queue by name |
| `s3:ListAllMyBuckets` | Each bucket by name and Region |
| `route53:ListHostedZones`, `route53:ListResourceRecordSets` | Each public hosted zone as a domain, and the host names its A, AAAA and CNAME records and aliases give, with what they resolve to |
| `iam:ListAccountAliases` | The account's alias, as its name |

From this the atlas also says which Regions are in use and what the account is used for (`compute`, `database`, `dns`, `object_storage`).

**What is never read.** No Lambda function (every listing action returns environment variables, so the role holds none); no instance user data, launch template, task definition, parameter, secret, object or log. Of what is listed, the atlas keeps no tag but `Name`, no database address, user or secret name, no private hosted zone, and no value of a record that is not an address or a name (TXT and MX values are never decoded).

**A refusal is a finding, never a failed read.** If an organization's policy closes a Region or service to the role, that part is listed as not read on the Sources tab, an earlier read's records stand, and the rest is read. Until the stack holds the role, the tab says the step above. A sign-in AWS refuses, or a listing that fails otherwise, fails the whole read and changes nothing.

**Cost.** AWS does not charge for these calls: four per enabled Region plus about one per hosted zone (a few dozen for most accounts); throttled calls retry with a doubling wait.

## Giving each agent its own role

The integration's **Agents** tab turns a seat on or off and sets its level, writing the seat's `roles[].integrations.aws` block:

| Field | Meaning |
|---|---|
| `enabled` | Whether the agent has its own role. A seat with no block is off. |
| `level` | `read`, `operate` or `administer`. Unset is `read`. |
| `grants` | AWS managed policies its role holds beside its level ([granting one more policy](#granting-one-more-policy)). Written by the Agents tab and by approving a request, never typed. |

```yaml
roles:
  - handle: sre
    integrations:
      aws:
        enabled: true
        level: operate
```

### What each change does in AWS

| You do | Crewlet does in IAM |
|---|---|
| Turn a seat on | Creates the role `crewlet-<name>-<seat>` under `/crewlet/<name>/seat/`, within the boundary, trusting only the broker under the agent's name; attaches its level's policies (a level attaches its own and every lower one) and the IAM read policy where that option is on. |
| Change its level | Attaches or detaches level policies on the same role, so CloudTrail keeps one identity. Applies within about 2 minutes. |
| Grant or remove a policy | Attaches or detaches that AWS managed policy on the same role, within the boundary. Applies within about 2 minutes. |
| Turn IAM read on or off | Attaches or detaches the IAM read policy on every agent that is on. |
| Turn it off | Attaches AWS's `AWSDenyAll` first, so the agent stops within about 2 minutes, then detaches its level policies. The role and its CloudTrail history stay. |
| Turn it back on | Attaches its level's policies, then detaches `AWSDenyAll`. |
| Remove it (only while off) | Detaches every policy and deletes the role, ending its sessions at once. |

`<seat>` is the agent's handle up to its first 20 characters plus 6 characters of its hash, so two agents never share a role.

Crewlet records each role by name and role ID and never adopts one by name: a same-named role it did not make is refused, with the step to delete it. A role with an inline policy, a customer managed policy that is not Crewlet's, or a trust Crewlet did not write is refused too (the provisioner may only attach and detach Crewlet's levels and AWS managed policies); remove what it names in IAM. An AWS managed policy nobody granted is detached by the next pass and reported.

### Granting one more policy

When an agent needs a permission no level holds, give it one AWS managed policy beside its level instead of a higher level:

- **On the Agents tab:** open the agent's row, choose **Add grant**, pick the policy from AWS's own list, and how long: 4 hours, 24 hours, 7 days, or permanent.
- **From the agent's request:** when AWS refuses a call with `AccessDenied`, the agent asks with `request_access`, naming the policy and why. You answer in your Inbox: **Grant for this change** (lapses after the hours shown), **Add to this agent permanently**, or **Not now**. Either grant shows the exact grant first; **Grant** approves it, and the engine saves it before telling the agent yes.

Rules:

- **The catalogue is AWS's.** It lists AWS's managed permissions policies (`iam:ListPolicies` with scope `AWS`), less those made for a service to act as itself and those no agent is ever given: `AdministratorAccess`, `PowerUserAccess`, and the IAM, IAM Identity Center, Organizations, account, billing, Secrets Manager, KMS and CloudTrail policies. Job function policies are listed under their own heading.
- **The boundary still holds.** A grant is attached within the seat boundary: only the services the levels name, never the actions the boundary denies, only in the company's Regions. A policy whose actions lie outside the boundary grants nothing.
- **A grant never outlives its expiry.** A pass runs a minute before it lapses and detaches it alone; the role and its other policies stay.
- **A policy whose name does not say `ReadOnly` or `ViewOnly` needs a fresh sign-in**, as `administer` does.
- **It needs the stack at version 3**, which lets the provisioner attach and list AWS managed policies, still within the boundary. Until the CloudShell block is run again, the Agents tab offers no grant and the card names the outdated stack.

```yaml
roles:
  - handle: sre
    integrations:
      aws:
        enabled: true
        level: read
        grants:
          - id: g7p3c8n2w5r
            permission: arn:aws:iam::aws:policy/AmazonSQSFullAccess
            expires_at: 2026-11-15T18:00:00Z
            granted_by: ada@example.com
            granted_at: 2026-11-15T14:00:00Z
            reason: Redrive the dead-letter queue after the incident
```

### Sessions are issued at use

Credentials are never stored. When a run or tool needs them, Crewlet signs a 15-minute certificate for the agent, opens the broker with it, and assumes the agent's role for **15 minutes**, with the session named for the agent (its source identity in CloudTrail). It renews with 5 minutes left and rewrites the box's credentials file. Each session is recorded before it is written there, so a [kept session](../concepts/code-sandbox.md#each-launchs-session-kept) of the run, taken once its job has ended, is scrubbed of it, whole and by every string it holds: access key ID, secret access key, session token, expiry. A renewal Crewlet cannot record because the coordination store did not answer is not written until it can be: the file keeps the session it holds, the poll retries every 15 seconds, and says so once (`sandbox_token_refresh_failed`, with the store's error).

After a seat is turned off and back on within 15 minutes, a session issued before it was turned off may work again until it expires.

### The agent's tools

**The AWS CLI.** The box holds two Crewlet-owned files and no AWS environment variable:

- `~/.aws/config`, with the profile `crewlet`, the home Region, and a `credential_process` that reads the file below. The default profile is never written, so commands use `--profile crewlet`. A box file of yours that also writes `~/.aws/config` replaces Crewlet's; keep your own profiles elsewhere.
- `~/.aws/crewlet-credentials.json`, the current session, reread by the CLI before it expires.

The AWS CLI v2 must be in the box image.

What a run prints of a session, this one or one it assumed itself, is redacted in the run's kept session: the access key ID by its shape, the secret access key and session token by the keys AWS writes them under (`SecretAccessKey`, `aws_session_token`, `AWS_SESSION_TOKEN` and their kind, as `aws sts assume-role`, `aws configure export-credentials` and the session file print them). A session Crewlet issued the seat, at launch or renewed since, is also removed by value, whole and by each string, so even printed bare, under no key (`jq -r .SessionToken`), it is gone; one the run assumed itself, printed bare, is kept as printed ([Code Sandbox](../concepts/code-sandbox.md#each-launchs-session-kept)). The run's live output shows the commands and a failed call's first line, never a command's output, redacted by the same shapes and keys but not by value ([Security model](../concepts/code-sandbox.md#security-model)).

**AWS's MCP Server.** Add it from the MCP catalogue (`aws`) as a per-agent server (`shared: false`), at the endpoint in your home Region where AWS runs one (us-east-1, us-west-2, eu-west-1, eu-west-2, eu-central-1, ap-southeast-1, ap-southeast-2 or ap-northeast-1), else us-east-1:

```yaml
mcp_servers:
  - name: aws
    transport: http
    shared: false
    url: "https://aws-mcp.eu-west-1.api.aws/mcp"
```

It reaches only agents that are on at AWS. Crewlet signs each request as the agent's role and adds the agent's home Region to each message, so no credential is entered, sent or stored, and AWS authorizes each call as the agent, as with the CLI. A box is never handed this server, since the session never enters it; the box's CLI is its way to AWS.

### Regions

Agents work in the company's Regions, enforced by the boundary: every other Region is refused, except global services such as IAM, Route 53 and CloudFront. The list is set in the company document only, and is the home Region and us-east-1 unless it names others:

```yaml
integrations:
  aws:
    provisioning:
      regions: [eu-west-1, us-east-1]
```

A changed list reaches AWS when the CloudShell block is run again; until then Test and every pass report the boundary as different from what this engine renders.

## Configuration

```yaml
integrations:
  aws:
    enabled: true
    provisioning:
      slug: acme
      home_region: eu-west-1
      regions: [eu-west-1, us-east-1]
      iam_read: false
      atlas_read: false
      certificate_authority: "${AWS_CERTIFICATE_AUTHORITY}"
      connection: "arn:aws:rolesanywhere:...,arn:aws:rolesanywhere:...,arn:aws:iam::...,arn:aws:rolesanywhere:...,arn:aws:iam::..."
```

| Field | Meaning |
|---|---|
| `slug` | The short name every Crewlet name in the account carries. |
| `home_region` | Where the stack, trust anchor and profiles live. |
| `regions` | The Regions agents may work in. Empty is the home Region and us-east-1. |
| `iam_read` | Whether every agent that is on can read IAM. |
| `atlas_read` | Whether the stack holds a read-only role for the atlas. Off by default. |
| `certificate_authority` | The sealed certificate authority, minted on Save. |
| `connection` | The stack's `CrewletConnection` output, comma-joined: trust anchor, provisioner's profile and role, agents' profile and the broker, then the atlas's profile and role where the stack holds them. Saving checks they are one stack's, in one account, with the trust anchor and profiles in the home Region. |

## AWS's limits worth knowing

| Limit | What it means here |
|---|---|
| IAM roles per account (1,000 by default) | One role per agent; Test warns when fewer than 20 are left. |
| Managed policies per role (10 by default) | An agent holds at most five of Crewlet's, leaving five for grants; a sixth is refused by AWS with `LimitExceeded`, named on the card. |
| Policy changes are eventually consistent | A level change or turn-off applies within about 2 minutes, as measured live. |
| A session lasts 15 minutes at least (STS's minimum) | Sessions are issued for 15 minutes and renewed with 5 left. An agent role's maximum session is an hour, IAM's minimum for a role. |

## What is not supported yet

- One AWS account per company; agents cannot be given several accounts.
- Regions per agent; the Regions are the company's.
- A template file to upload in the CloudFormation console, or a link that opens it: the CloudShell block is the one way to create the stack.
- Events from AWS (CloudWatch alarms, EventBridge) reaching an agent.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Test: "AWS refused Crewlet's sign-in" | The trust anchor lacks this engine's certificate (a new one was minted, or the output is another stack's). Run the CloudShell block again in the right account. |
| Test: "the stack is version ..." or "is not the one this engine expects" | The stack is older than this engine, or was edited by hand. Run the CloudShell block again. A stack made before version 3 reports this until the block is run once: version 2 narrows what Read can read of Lambda, version 3 lets the provisioner attach and list AWS managed policies for grants. |
| Test: "The stack output does not carry the Atlas reader" | **See more in Atlas** is on and the block has not been run since, or its last line was not pasted again. Run the block and paste the last line. Agents are not affected. |
| An agent's lane: "already exists, and Crewlet has no record of making it" | A role of the agent's name exists that Crewlet did not create. Delete it in IAM if nobody uses it. |
| An agent's lane: "also holds ..., which no level grants" | Someone attached a policy to the agent's role. Remove it in IAM. |
| An agent's lane: "AWS is still applying the change" | A level or new role is propagating through IAM; it clears within a few minutes. |
| `aws` commands fail with "Unable to locate credentials" | The command did not use the profile: add `--profile crewlet`. |

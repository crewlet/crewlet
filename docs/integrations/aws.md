# AWS

AWS gives each agent its **own IAM role** in your account, at the level you choose, and signs it in for 15 minutes at a time, so CloudTrail names the agent behind every call it makes. An agent works through the **AWS CLI** in its box and through **AWS's MCP Server**, both as its own role.

Connecting creates one CloudFormation stack in your account. Crewlet stores no access key: it signs in through **IAM Roles Anywhere** with a certificate authority it holds itself, whose certificate the stack registers. Nothing here receives events from AWS yet; see [What is not supported yet](#what-is-not-supported-yet).

## What agents get

| An agent on | Can | Cannot |
|---|---|---|
| `read` (the default) | Describe and list EC2 (with VPC), Auto Scaling, Elastic Load Balancing, ECS, EKS, RDS, Route 53 and CloudFront; read how a Lambda function is wired (its aliases, event sources, URL, policy and concurrency) without listing functions or reading a function's configuration; read CloudWatch metrics and CloudWatch Logs; list bucket and object names; read Systems Manager inventory and parameter names; look up CloudTrail events; read ECR, ACM and KMS metadata, tags and AWS Health | Read objects, items, parameter values or secrets, or change anything |
| `operate` | Everything `read` can, plus start, stop and reboot instances; scale Auto Scaling groups, ECS services and EKS node groups; invoke functions; reboot, start, stop and snapshot databases; read, write and delete S3 objects; register load balancer targets; invalidate CloudFront; edit CloudWatch alarms and dashboards; read plain Systems Manager parameters | Create key pairs, user data, database passwords, bucket or key policies, DNS records, security group changes, new code or task definitions |
| `administer` | Everything `operate` can, plus manage those services whole | See below |

**Administer can create access that outlives the agent's session:** instance key pairs, user data on instances, database users, bucket and key policies, function URLs, security group openings and snapshot sharing. Granting it takes a fresh sign-in. Moving an agent off Administer leaves whatever it created; review and remove it.

**What no level can ever do,** because the stack's permissions boundary refuses it to every agent role: write IAM or pass a role, assume another role or mint another credential, read a secret's value, manage KMS keys, touch billing, the organization or the account, administer an EKS cluster's access, or stop or change CloudTrail. An agent works only in the company's Regions (see [Regions](#regions)).

**Acting as a workload's identity is gated.** Starting a shell or a command on an instance (Session Manager, EC2 Instance Connect), running an ECS task or command, or changing a function's code acts with that workload's own role, so Crewlet allows it only on resources you tag `crewlet:agent-access = administer`, and only at Administer.

**Read includes CloudWatch Logs**, which hold whatever your workloads print. A secret a workload writes to its logs, to its user data, or to a task definition's environment variables is readable at Read. A function's environment variables are not: Read cannot list functions or read a function's configuration, since both answer its environment, so an agent at Read finds a function by its log group or its tags and reads its configuration only at Operate.

The company-wide **Let agents read IAM** option adds `iam:Get*` and `iam:List*` (the account's users, groups, roles and policies) to every agent that is on. It is off by default, since it shows the people in your account.

Every agent's role is its own, so CloudTrail names the agent behind each call, and turning one agent off never touches another.

## Setting it up from the dashboard

Company › Integrations › AWS asks, in order:

1. **Short name**: the name every Crewlet object in your account carries, as `crewlet-<name>`. It is offered from the company's name, holds up to 20 lowercase letters, digits and hyphens, and cannot change once the stack exists.
2. **Home Region**: where the stack and Crewlet's sign-in live. The list is every Region IAM Roles Anywhere is available in.
3. **Let agents read IAM**: the option above, off by default.
4. **See more in Atlas**: optional and off by default. It adds a read-only role for the atlas to the stack ([what the atlas reads](#what-the-atlas-reads)). No agent depends on it.

**Save** mints the **sign-in certificate**: a certificate authority whose key Crewlet seals as `AWS_CERTIFICATE_AUTHORITY` and never shows. The form then shows the **CloudShell block**, which carries only the authority's certificate.

5. Sign in to the AWS account agents will work in, open CloudShell in the home Region, and paste the block. It writes the stack's template, deploys it with `aws cloudformation deploy` (you need permission to create IAM roles and policies), and prints the stack's one output last.
6. Paste that last line into **Stack output** and choose **Test**.

Running the block again updates the stack in place, which is also how a new version of Crewlet's policies, a change of Regions, or the atlas's role reaches your account. Paste the last line again whenever the block changed what the stack holds.

### What the stack creates

| Object | Name | What it is for |
|---|---|---|
| Stack | `crewlet-<name>` | Holds everything below; deleting it removes Crewlet's access |
| Trust anchor | `crewlet-<name>` | Holds Crewlet's certificate, so Crewlet's sign-ins are accepted |
| Provisioner role and profile | `crewlet/<name>/crewlet-<name>-provisioner` | Creates, levels, turns off and deletes the agents' roles, only inside the boundary, and nothing else |
| Broker role and profile | `crewlet/<name>/crewlet-<name>-broker` | Becomes one agent's role at a time, and can do nothing else |
| Permissions boundary | `crewlet-<name>-seat-boundary` | The most any agent role can ever do |
| Level policies | `crewlet-<name>-level-read`, `-level-operate`, `-level-administer`, `-level-iam-read` | What each level grants |
| Atlas role and profile (only with **See more in Atlas**) | `crewlet/<name>/crewlet-<name>-atlas` | Lists what the account runs for the atlas, read-only, and nothing else |

Every object is tagged `crewlet:managed-by = crewlet`, `crewlet:company = <name>` and `crewlet:stack-version`. Agents' roles live under the path `/crewlet/<name>/seat/`.

The provisioner, the broker and the atlas's role are separate on purpose. The atlas's certificate can only become the atlas's role, and neither of the other two can become it.

The provisioner and the broker are separate from each other for the agents' sake. Crewlet's certificate for the provisioner can only become the provisioner, which cannot become any agent. An agent's sign-in can only become the broker, which can only become that agent's own role: each role trusts the broker only in a session named for that agent.

### Testing before you save

**Test** signs in as the provisioner and reads, without changing anything:

| Check | What a failure means |
|---|---|
| The sign-in | The trust anchor does not hold this engine's certificate, or the output is another stack's |
| The stack's version | The stack was made by another version of Crewlet |
| Every policy and role | The boundary, a level, or the provisioner's or the broker's trust or policy differs from what this engine renders: someone edited it, or the Regions changed |
| The identity split | An agent's certificate could become the provisioner, or the provisioner's the broker; with the atlas's role, that its certificate could become either, or either's the atlas |
| The atlas's role (only with **See more in Atlas**) | The stack or the pasted output does not hold the role yet, or its trust or policy differs. This is a warning beside a connection that works, never a refusal |

Each failure's fix is the same: run the CloudShell block again, which updates the stack, and do not edit Crewlet's roles or policies by hand. When every check passes, the result names the account's IAM role count against its quota, and warns when fewer than 20 roles are left. Every pass afterwards makes the same check, and a difference is a finding on the integration's card.

## What the atlas reads

Without anything further, the [atlas](../concepts/atlas.md) shows the AWS account and the Regions your agents work in, from the settings alone. **See more in Atlas** lets it list what the account runs. It is optional, nothing waits on it, and it never changes what an agent can do.

**The one step.** Turn **See more in Atlas** on and save, run the CloudShell block again, and paste its last line into **Stack output**. The block now creates a role and a profile for the atlas, and the last line holds seven ARNs where it held five. In the company document the switch is `integrations.aws.provisioning.atlas_read`.

**To undo it.** Turn the switch off, save, and run the block again: CloudFormation deletes the role and the profile. Turning the switch off stops the reads at once, before the block is run. Deleting the role in IAM by hand stops them too, and the atlas's Sources tab then says the read failed. What the atlas already recorded stays until a person retires it.

**Who reads.** The engine signs in as the role `crewlet-<name>-atlas`, through the same trust anchor as the rest of the stack and with a certificate named for the atlas, once an hour unless `atlas.refresh_interval_seconds` says otherwise, and when somebody presses **Refresh now**. CloudTrail shows these calls under that role, with the source identity `CN=crewlet-<name>-atlas`, never under an agent's.

**What the role may do.** Its policy allows nine actions and denies every other, so a policy attached to the role by hand widens nothing:

| Action | What the atlas gains |
|---|---|
| `ec2:DescribeRegions` | The Regions enabled for the account, each of which is then asked |
| `ec2:DescribeInstances` | Each instance as a server: its id, its `Name` tag, its private and public addresses and its Region. A terminated instance is left out |
| `rds:DescribeDBInstances` | Each database instance: its identifier, its engine (`postgres`, `mysql`, `mariadb` or `other`), its engine version and its port |
| `elasticloadbalancing:DescribeLoadBalancers` | Each application, network and gateway load balancer as the host name it answers on, which is what a DNS record points at. A Classic Load Balancer is not listed |
| `sqs:ListQueues` | Each queue by its name |
| `s3:ListAllMyBuckets` | Each bucket by its name and Region |
| `route53:ListHostedZones`, `route53:ListResourceRecordSets` | Each public hosted zone as a domain, and the host names its A, AAAA and CNAME records and aliases give, with what they resolve to |
| `iam:ListAccountAliases` | The account's alias, as its name |

From what it finds, the atlas also says which Regions are in use and what the account is used for (`compute`, `database`, `dns`, `object_storage`).

**What is never read.** No Lambda function: every action that lists functions returns their environment variables, so the role holds none. No instance user data or launch template, no task definition, no parameter, secret, object or log. Of what is listed, the atlas keeps no tag but `Name`, no database address, user or secret name, no private hosted zone, and no value of a record that is not an address or a name (a TXT or MX record's value is never decoded).

**A refusal is a finding, never a failed read.** If an organization's policy closes a Region or a service to the role, that part is listed as not read on the atlas's Sources tab, what an earlier read recorded there stands, and everything else is read. Until the stack holds the role, the tab says the step above. A sign-in AWS refuses, or a listing that fails for any other reason, fails the read whole and changes nothing.

**Cost.** AWS does not charge for these calls. A read makes four calls per enabled Region and about one per hosted zone, a few dozen for most accounts, and throttled calls are retried with a doubling wait.

## Giving each agent its own role

Each agent seat is turned on or off, and given its level, on the integration's **Agents** tab. The tab writes the seat's `roles[].integrations.aws` block:

| Field | Meaning |
|---|---|
| `enabled` | Whether the agent has its own role. A seat with no block is off. |
| `level` | `read`, `operate` or `administer`. Unset is `read`. |

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
| Turn a seat on | Creates the role `crewlet-<name>-<seat>` under `/crewlet/<name>/seat/`, within the boundary and trusting only the broker under the agent's name, then attaches its level's policies (a level attaches its own policy and every lower one) and the IAM read policy where that option is on. |
| Change its level | Attaches or detaches level policies on the same role, so CloudTrail keeps one identity. It applies within about 2 minutes. |
| Turn IAM read on or off | Attaches or detaches the IAM read policy on every agent that is on. |
| Turn it off | Attaches AWS's `AWSDenyAll` policy first, so the agent stops within about 2 minutes, then detaches its level policies. The role and its CloudTrail history stay. |
| Turn it back on | Attaches its level's policies, then detaches `AWSDenyAll`. |
| Remove it (only while off) | Detaches every policy and deletes the role, which ends its sessions at once. |

`<seat>` is the agent's handle, up to its first 20 characters, and 6 characters of its hash, so two agents never share a role.

Crewlet records each role by name and role ID and never adopts a role by its name: a role of the same name that it did not make is refused, with the step to delete it. A role holding an inline policy, a policy that is not Crewlet's, or a trust Crewlet did not write is refused too, since the provisioner may only attach and detach Crewlet's own levels; remove what it names in IAM.

### Sessions are issued at use

An agent's credentials are never stored. When a run or a tool needs them, Crewlet signs a 15-minute certificate for the agent, opens the broker with it, and assumes the agent's role for **15 minutes**, naming the session after the agent, which is also its source identity in CloudTrail. The session is renewed with 5 minutes left, and the box's credentials file is rewritten as it renews. Each session is recorded before it is written there, so a [kept session](../concepts/code-sandbox.md#each-launchs-session-kept) of the run, taken once its job has ended, is scrubbed of it, whole and by every string it holds: its access key ID, its secret access key, its session token and its expiry. A renewed session Crewlet cannot record, because the coordination store did not answer, is not written until it can be: the file keeps the session it holds, the poll tries again every 15 seconds, and it says so once (`sandbox_token_refresh_failed`, with the store's error).

After a seat is turned off and back on within 15 minutes, a session issued before it was turned off may work again until it expires.

### The agent's tools

**The AWS CLI.** The box holds two files Crewlet owns and no AWS environment variable:

- `~/.aws/config`, with the profile `crewlet`, the home Region, and a `credential_process` that reads the file below. Crewlet never writes the default profile, so commands use `--profile crewlet`. A box file of yours that also writes `~/.aws/config` replaces Crewlet's; keep your own profiles elsewhere.
- `~/.aws/crewlet-credentials.json`, the current session, which the CLI reads again before it expires.

The AWS CLI v2 must be in the box image.

What the run prints of a session, this one or one it assumes itself, is kept in the run's own session redacted: its access key ID by its shape, and its secret access key and session token by the keys AWS writes them under — `SecretAccessKey`, `aws_session_token`, `AWS_SESSION_TOKEN` and their kind, as `aws sts assume-role`, `aws configure export-credentials` and the session file itself print them. A session Crewlet issued the seat, at its launch or renewed since, is also taken out by value, whole and by each of its strings, so even printed bare, under no key (`jq -r .SessionToken`), it is gone; a session the run assumed by itself, printed bare, is kept as printed ([Code Sandbox](../concepts/code-sandbox.md#each-launchs-session-kept)). The run's live output shows the commands it runs and a failed call's first line, never a command's output, redacted by the same shapes and keys, though never by value ([Security model](../concepts/code-sandbox.md#security-model)).

**AWS's MCP Server.** Add it from the MCP catalogue (`aws`) as a per-agent server (`shared: false`), at the endpoint in your home Region where AWS runs one (us-east-1, us-west-2, eu-west-1, eu-west-2, eu-central-1, ap-southeast-1, ap-southeast-2 or ap-northeast-1), else us-east-1:

```yaml
mcp_servers:
  - name: aws
    transport: http
    shared: false
    url: "https://aws-mcp.eu-west-1.api.aws/mcp"
```

It reaches only the agents that are on at AWS. Crewlet signs each request as the agent's own role and adds the agent's home Region to each message, so no credential is entered, sent or stored, and AWS authorizes each AWS call a tool makes as the agent, exactly as the CLI's calls are. A box is never handed this server, since the session never enters it; the box's CLI is its way to AWS.

### Regions

Agents work in the company's Regions, which the stack's boundary enforces: every other Region is refused, except for global services such as IAM, Route 53 and CloudFront. The list is set in the company document only, and is the home Region and us-east-1 unless it names others:

```yaml
integrations:
  aws:
    provisioning:
      regions: [eu-west-1, us-east-1]
```

A changed list reaches AWS when the CloudShell block is run again. Until then, Test and every pass report the boundary as different from what this engine renders.

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
| `home_region` | Where the stack, its trust anchor and its profiles live. |
| `regions` | The Regions agents may work in. Empty is the home Region and us-east-1. |
| `iam_read` | Whether every agent that is on can read IAM. |
| `atlas_read` | Whether the stack holds a read-only role for the atlas. Off by default. |
| `certificate_authority` | The sealed certificate authority, minted on Save. |
| `connection` | The stack's `CrewletConnection` output: the trust anchor, the provisioner's profile and role, the agents' profile and the broker, then the atlas's profile and role where the stack holds them, joined by commas. Saving checks that they are one stack's, in one account, with the trust anchor and profiles in the home Region. |

## AWS's limits worth knowing

| Limit | What it means here |
|---|---|
| IAM roles per account (1,000 by default) | One role per agent; Test warns when fewer than 20 are left. |
| Managed policies per role (10 by default) | An agent holds at most five of Crewlet's. |
| Policy changes are eventually consistent | A level change or a turn-off applies within about 2 minutes, as measured live. |
| A session lasts 15 minutes at least (STS's minimum) | Sessions are issued for 15 minutes and renewed with 5 left. An agent role's maximum session is an hour, IAM's minimum for a role. |

## What is not supported yet

- One AWS account per company; agents cannot be given several accounts.
- Regions per agent; the Regions are the company's.
- A template file to upload in the CloudFormation console, or a link that opens it: the CloudShell block is the one way to create the stack.
- Events from AWS (CloudWatch alarms, EventBridge) reaching an agent.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Test: "AWS refused Crewlet's sign-in" | The trust anchor does not hold this engine's certificate (a new certificate was minted, or the output is another stack's). Run the CloudShell block again in the right account. |
| Test: "the stack is version ..." or "is not the one this engine expects" | The stack is older than this engine, or was edited by hand. Run the CloudShell block again. A stack made before version 2 reports this until the block is run once: version 2 narrows what Read can read of Lambda. |
| Test: "The stack output does not carry the Atlas reader" | **See more in Atlas** is on and the block has not been run since, or its last line was not pasted again. Run the block and paste the last line. Agents are not affected. |
| An agent's lane: "already exists, and Crewlet has no record of making it" | A role of the agent's name exists that Crewlet did not create. Delete it in IAM if nobody uses it. |
| An agent's lane: "also holds ..., which no level grants" | Someone attached a policy to the agent's role. Remove it in IAM. |
| An agent's lane: "AWS is still applying the change" | A level or a new role is propagating through IAM; it clears within a few minutes. |
| `aws` commands fail with "Unable to locate credentials" | The command did not use the profile: add `--profile crewlet`. |

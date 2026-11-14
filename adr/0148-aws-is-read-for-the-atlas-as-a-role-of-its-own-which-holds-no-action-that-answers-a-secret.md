# ADR-0148: AWS is read for the atlas as a role of its own, which holds no action that answers a secret

- **Status:** accepted
- **Authority:** `internal/aws`
- **Enforced-by:** `internal/aws.TestTheAccountIsReadAsTheAtlasDrawsIt`, `internal/aws.TestTheReadAsksOnlyWhatTheRoleAllowsAndTheRoleAllowsOnlyThat`, `internal/aws.TestTheAtlasRoleNamesEveryActionAndNoneAnswersASecret`, `internal/aws.TestReadReadsNoFunctionConfiguration`, `internal/aws.TestNoUnmappedFieldReachesTheAtlasFromAWS`, `internal/aws.TestNoCredentialShapeReachesTheAtlasFromAWS`, `internal/aws.TestARefusedListingIsUnreadAndItsEarlierClaimsStand`, `internal/aws.TestAFaultOrARefusedSignInFailsTheReadWhole`, `internal/aws.TestTheReaderIsOffUntilAskedForAndSaysTheStepWhenTheStackLacksIt`, `internal/aws.TestTheStackHoldsTheAtlasRoleOnlyWhenAskedFor`, `internal/aws.TestTheCheckHoldsTheAtlasRoleApartFromTheAgents`, `internal/aws.TestTheConnectFormOffersTheAtlasReader`
- **Cost-when-tried:** not tried. The provisioner reads only IAM, and an agent's session is named for the agent in CloudTrail, so neither identity the stack held could read the account for the atlas honestly.
- **Tag-status:** unreleased

## The decision

**The atlas reads AWS as a third identity in the stack, and only where the company asks.** `integrations.aws.provisioning.atlas_read`, off by default, adds a role `crewlet-<slug>-atlas` and a Roles Anywhere profile that offers that role alone to the stack the engine renders. Its trust admits one certificate name, the atlas's own, under the same certificate authority and trust anchor as the provisioner and the broker, so CloudTrail names the atlas behind every call and never an agent. Rendered without the switch, the stack creates neither, so running the CloudShell block again removes them. The setup step is the one the connection already has: run the block and paste its last line.

**The stack output carries the role where the stack holds it.** `CrewletConnection` is five ARNs, or seven with the atlas's profile and role last. The engine reads the profile's ARN from there because a profile's ARN is minted by AWS and cannot be derived, and because giving the provisioner a way to list profiles would widen the identity that makes agents' roles.

**The role's policy names nine actions and denies every other.** `ec2:DescribeRegions`, `ec2:DescribeInstances`, `rds:DescribeDBInstances`, `elasticloadbalancing:DescribeLoadBalancers`, `sqs:ListQueues`, `s3:ListAllMyBuckets`, `route53:ListHostedZones`, `route53:ListResourceRecordSets` and `iam:ListAccountAliases`. None is a wildcard, and a second statement denies everything that is not one of them, so a policy attached to the role by hand widens nothing. One test holds the list to the requests the read makes, in both directions.

**No action that answers a secret is in the list, so functions are not read.** Every Lambda action that lists functions or reads one (`ListFunctions`, `ListVersionsByFunction`, `GetFunction`, `GetFunctionConfiguration`) answers the function's environment, and no other action answers its runtime. A decoder that skips the environment would still hold a permission that returns it. The same rule keeps out instance attributes (user data), launch templates, task definitions, parameters, objects and logs.

**The agents' Read level is held to the same rule for functions.** It no longer grants `lambda:GetFunctionConfiguration` or a `lambda:List*` wildcard. It names the Lambda listings that say how a function is wired (aliases, event source mappings, URL and invoke configurations, layers, tags, the runtime management setting) and none that answers a function's configuration. The stack's version moves from 1 to 2 with it.

**The reader is a client of its own, with closed decoders.** Each listing is a signed request decoded into the fields the atlas keeps: an instance's id, state, addresses and `Name` tag; a database's identifier, engine, engine version, status and port; a load balancer's DNS name; a queue's name; a bucket's name and Region; a public hosted zone's name; and the name, type and target of an A, AAAA or CNAME record set. A database's address, master user and secret, every other tag, and the value of every other record type are never decoded.

**What the read records.** The account (the key the settings give it) with its alias, the Regions something was found in and what it is used for (`compute`, `database`, `dns`, `object_storage`); each instance as a host, each database as a datastore with its engine in the atlas's closed words, each queue and bucket by name, each load balancer as the host name a record points at, and each public zone as a domain with the names its records give and what they resolve to. An alias to a load balancer meets the balancer's own name. A private hosted zone is not read, since a name that answers only inside a network would be recorded as the public name it shadows.

**Each listing of each Region is a part of its own.** The listing of Regions proves the sign-in and fails the read if AWS refuses it. After it, a listing answered `AccessDenied` or `UnauthorizedOperation` is unread for that Region (an organization's policy may close a Region or a service to every role), the standing counts the Regions, and ADR-0130 carries what an earlier read recorded there. A refusal is read by its code, never by a `403` alone, which AWS also answers for a session it does not accept. Any other failure fails the read whole.

**The role never stands between an agent and its work.** The reconcile pass and the Test button hold the role to the identity split (no other certificate opens it, and it opens no other role). What it otherwise lacks is a warning beside a connection that works, and a finding on the atlas's own source, never drift on the integration.

## Why the obvious alternative is wrong

**Reading as an agent's Read session** reads everything the atlas needs today, and CloudTrail would show an agent listing the whole account every hour.

**Widening the provisioner** puts read access to the account's resources on the identity that creates roles, which the stack keeps to IAM alone.

**`lambda:ListFunctions` with a decoder that drops the environment** records nothing secret and still returns every function's environment to the engine's process on every read. The rule is about what the permission can return, not what the decoder keeps.

**Always creating the role** makes an optional reader a part of every company's stack, with no way to decline it short of editing the stack by hand, which the check then reports.

**The AWS SDK's service clients** decode every field of every answer before the read can drop it, and add five service modules to a binary whose every linked package's init is paid on each Gitlet push.

**Deriving the profile's ARN, or listing profiles as the provisioner,** is not possible for the first and widens the provisioner for the second.

**Reporting a missing role as drift** tells a company its agents' access needs attention when only an optional reader does.

## What this does not decide

Whether functions are read by another route. Whether more than one account is read. What is read of containers, clusters, distributions or certificates, each of which takes an action added to the list and the stack run again. Whether the agents' Read level keeps `ec2:Describe*`, `autoscaling:Describe*` and `ecs:Describe*`, which reach user data and task definitions and which `docs/integrations/aws.md` tells its reader about.

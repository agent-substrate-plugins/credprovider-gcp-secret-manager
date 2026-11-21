# How to Contribute

We would love to accept your patches and contributions to this project. Before
you spend a lot of time on a contribution, please review the following
guidelines.

## Before you begin

### Sign off your commits

Every commit must be signed off under the
[Developer Certificate of Origin](https://developercertificate.org/) (DCO).
The `Signed-off-by` line certifies that you wrote the change or otherwise have
the right to submit it under the project's license.

Sign off with your real name, not an anonymous ID or a false name. It doesn't
need to be your legal name, just the name the community knows you by. See the
[CNCF DCO guidelines](https://github.com/cncf/foundation/blob/main/policies-guidance/dco-guidelines.md).

Git adds the line with `-s`, using `user.name` and `user.email` from your git
config. The name and email must match the commit author.

```sh
git commit -s -m "fix typo in README"
```

The DCO check on your PR fails if any commit is missing a sign-off. To sign off
the commits already on your branch, then force-push:

```sh
git rebase --signoff upstream/main
```

### Code of conduct

Please read and internalize our [code of conduct](CODE_OF_CONDUCT.md). We want
this to be a welcoming and inclusive community, and we expect everyone to
adhere to the code of conduct at all times. If you see someone violating the
code of conduct, please report it to the maintainers immediately. We take all
reports seriously and will take appropriate action to address any issues.

Open, direct, honest communication is encouraged, but always be respectful and
professional.

### Follow the Agent Substrate project policies

This repository is part of the Agent Substrate project and shares its
maintainers, governance and collaboration norms, including the AI contribution
policy. See
[COLLABORATING.md](https://github.com/agent-substrate/substrate/blob/main/COLLABORATING.md),
[GOVERNANCE.md](https://github.com/agent-substrate/substrate/blob/main/GOVERNANCE.md)
and
[MAINTAINERS.md](https://github.com/agent-substrate/substrate/blob/main/MAINTAINERS.md)
in [agent-substrate/substrate](https://github.com/agent-substrate/substrate).

## Contribution process

### Code Reviews

All submissions, including submissions by project members, require review. We
use [GitHub pull requests](https://docs.github.com/articles/about-pull-requests)
for this purpose.

Changes that add or alter behavior should come with tests, and existing tests
should be updated when behavior they cover changes. We will not merge code that
causes tests to fail. Every test runs in CI on every PR, so please don't add
tests whose only purpose is to raise coverage numbers. Tests should be
inherently valuable for future changes.

### Copyright Headers

Every file containing source code must include copyright and license
information. This includes any JS/CSS files that you might be serving out to
browsers. (This is to help well-intentioned people avoid accidental copying
that doesn't comply with the license.)

Our standard headers for various filetypes can be found in
[./boilerplate](./boilerplate/).

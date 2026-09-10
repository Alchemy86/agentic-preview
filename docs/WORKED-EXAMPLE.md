# A worked example, end to end

The pattern, with a fictional `checkout-api` in a namespace called `shop` and a made-up
work id of `1234`. Substitute your own — and note that `1234` is *any string you like*: an
issue number, a branch name, `blue-widget`, somebody's initials. agentic-preview uses it as
the header value and as a label, and never reads anything out of it.

**1. Somebody opens a pull request** against `checkout-api`.

**2. Your existing CI builds the service's image.** It almost certainly does this already —
building the image is how most pipelines run their tests against the real artifact — and
then throws it away. *That is the insight that makes this cheap.* You are not adding a
build; you are keeping one you were already paying for. Whatever you run this in — GitHub
Actions, GitLab CI, Jenkins, Buildkite, a shell script on a box — is where step two already
happens. None of them is required; agentic-preview never sees this step.

**3. Push that image with a tag carrying an id of your choosing.** `pr-1234`,
`branch-blue-widget`, whatever your conventions are. The shape of that tag is entirely
yours: agentic-preview never reads it, never parses it, and never assumes a registry.

**4. Call this tool** with the service, the namespace, that id and that image:

```bash
curl -XPOST http://agentic-preview.agentic-preview.svc.cluster.local/previews \
  -H 'content-type: application/json' \
  -d '{"workId":"1234","workload":"checkout-api","namespace":"shop",
       "image":"registry.example.com/checkout-api:pr-1234","port":"http"}'
```

It copies the live `checkout-api`, runs your image with the live configuration, puts a
Service in front of it, and makes the header live. If the change spans more repositories,
each one calls this as it builds, with the same `workId` — and one header then reaches all
of them.

**5. A request carrying the header reaches the preview. Everything else reaches live.**

```bash
curl -H 'x-preview: 1234' https://your-ingress/checkout   # the PR build
curl                      https://your-ingress/checkout   # live, untouched
```

**6. When you are finished with it, something calls `DELETE`** and the preview goes.

```bash
curl -XDELETE http://agentic-preview.agentic-preview.svc.cluster.local/previews/1234
```

**A merge is deliberately not the trigger**, and this is the assumption most readers arrive
with. You may well still be testing against the preview after the branch has merged —
comparing behaviour, reproducing something, showing somebody. Teardown is explicit:
somebody asks, and it goes. The only thing that removes a preview on its own is the
[lifetime safety net](LIMITS.md#there-is-a-timer-against-forgotten-previews-and-you-may-well-want-it-off),
and that can be switched off.

## Which of those six steps are this tool

**Four and five.** Building the preview from the live workload, creating it, routing the
header to it, and forwarding the traffic — that is agentic-preview, and that is all of it.

**One, two, three and six are yours.** Opening the pull request, building the image,
pushing it under a tag you chose, and deciding when you are done with the preview. Step
three's tag shape in particular is entirely yours: the tool receives a reference and runs
it. *Triggering is the adopter's job; the tool only receives calls.*

## The API in full

Every field, every response and every endpoint is in
[Design → The API](DESIGN.md#the-api). Runnable versions of all five operations are in
[`examples/`](../examples/) — plain `curl`; the API is small enough that a wrapper would
only hide it.

### Raising, rolling and adding

POSTing the same work id again **adds** a service to it. POSTing the same work id *and*
service with the same image is a no-op (`"unchanged"`), so a pipeline retry is safe; with a
**new** image it rolls the Deployment forward in place and leaves the intercept alone
(`"rolled"`), so a new commit is one call and no gap in routing.

If the preview's pods do not come up you get the reason — `ErrImagePull: manifest unknown`,
`ImagePullBackOff` — and no intercept, rather than a header that hangs. The objects are
left in place so you can look at them.

### Deploying the preview yourself

Send `previewService` instead of `image` and nothing is built; agentic-preview does the
intercept half only, exactly as it did before it could build anything. That is the right
shape when the preview is not a copy of one live Deployment.

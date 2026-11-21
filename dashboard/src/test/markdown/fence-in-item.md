# A fenced block inside a numbered step

Copy the pair, then make the edits to the company copy:

1. **The axis.** Set `chat.backend: vendor` and delete the `chat.chatlet`
   block. `typing_status: addressed` moves to the Mattermost block below.
2. **The workspace.** Add the block the bootstrap prints, under a new
   `integrations:` key:

   ```yaml
   integrations:
     mattermost:
       enabled: true
       url: "${MATTERMOST_URL}"
       provisioning:
         channels: [leadership, product, engineering]
   ```

   Leave `username_prefix` empty: the handles already start with `agent-`,
   and a prefix would produce `@agent-agent-ceo`.
3. **How a seat sends.** Add the tool server under a new `mcp_servers:` key:

   ```yaml
   mcp_servers:
     - name: mattermost
       shared: false
       command: uvx
   ```

   It is pinned to a release ([see the catalogue](https://example.com/catalogue)),
   so a new release is read before it is run.

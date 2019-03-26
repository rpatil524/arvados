# Copyright (C) The Arvados Authors. All rights reserved.
#
# SPDX-License-Identifier: AGPL-3.0

def diff_hash base, final
  diffed = {}
  base.each do |k,v|
    bk = base[k]
    fk = final[k]
    if bk.is_a? Hash
      d = diff_hash bk, fk
      if d.length > 0
        diffed[k] = d
      end
    else
      if bk.to_s != fk.to_s
        diffed[k] = fk
      end
    end
  end
  diffed
end

namespace :config do
  desc 'Diff site configuration'
  task diff: :environment do
    diffed = diff_hash $base_arvados_config, $arvados_config
    cfg = { "Clusters" => {}}
    cfg["Clusters"][$arvados_config["ClusterID"]] = diffed.select {|k,v| k != "ClusterID"}
    puts cfg.to_yaml
  end
end

<?php

namespace App\Pager;

use Pagerfanta\Adapter\DoctrineORMAdapter;

/**
 * This adapter don't make COUNT() queries.
 */
class PagelessDoctrineORMAdapter extends DoctrineORMAdapter
{
    public function getNbResults()
    {
        return 1;
    }
}

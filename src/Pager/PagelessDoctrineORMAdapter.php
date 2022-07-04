<?php
declare(strict_types=1);

namespace App\Pager;

use Pagerfanta\Doctrine\ORM\QueryAdapter;

/**
 * This adapter don't make COUNT() queries.
 */
class PagelessDoctrineORMAdapter extends QueryAdapter
{
    public function getNbResults(): int
    {
        return 1;
    }
}

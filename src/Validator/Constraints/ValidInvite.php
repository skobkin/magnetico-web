<?php

namespace App\Validator\Constraints;

use Symfony\Component\Validator\Constraint;

/**
 * @Annotation
 */
class ValidInvite extends Constraint
{
    public $notFoundMessage = 'Invite {{ code }} not found.';
    public $usedMessage = 'Invite {{ code }} is used.';

    public function validatedBy(): string
    {
        return get_class($this).'Validator';
    }
}
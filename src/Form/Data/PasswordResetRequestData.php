<?php

namespace App\Form\Data;

use Symfony\Component\Validator\Constraints as Assert;

class PasswordResetRequestData
{
    /**
     * @var string
     *
     * @Assert\Email()
     * @Assert\NotBlank()
     */
    public $email;
}
